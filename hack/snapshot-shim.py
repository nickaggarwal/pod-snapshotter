#!/usr/bin/env python3
"""Reference quiesce/resume shim for pod-snapshotter (docs/design-v2.md §4).

The contract, in full:

  1. The workload initializes — weights loaded, kernels warm, CUDA graphs
     captured — and then releases whatever it can afford to lose.
  2. It creates ``<quiesce-dir>/ready-for-checkpoint``.
  3. It blocks, polling for ``<quiesce-dir>/restore-complete``.
     *The checkpoint happens here*, while the process sits in the poll loop.
  4. On restore, CRIU resumes execution at the exact instruction inside that
     loop. The resume file is already present (the node agent writes it before
     ``runc restore`` returns).
  5. The workload reacquires what it released and only *then* opens its
     listening sockets and joins the distributed runtime.

Step 5 is the whole point. Everything created after the quiesce point —
io_uring rings, the HTTP listener with its live peers, NCCL communicators,
RDMA registrations — is absent from the CRIU image, so CRIU never has to dump
what it cannot dump, and the resume side builds those structures fresh on a
node where they can be built correctly.

Opt in by annotating the pod::

    metadata:
      annotations:
        podsnapshot.io/quiesce: "presence-file"
        podsnapshot.io/quiesce-dir: "/snapshot"     # default
        podsnapshot.io/quiesce-timeout: "900s"      # default 10m

``<quiesce-dir>`` must be a writable volume (an emptyDir); it is captured in
the image, and the agent writes the resume file into the restored pod's copy.

Usage::

    snapshot-shim.py [shim options] -- vllm serve args...
    snapshot-shim.py --self-test          # no GPU: exercises the protocol

This file is a reference, not a dependency: any workload that creates the
presence file and blocks on the resume file works. Nothing here is imported
by pod-snapshotter.
"""

from __future__ import annotations

import argparse
import inspect
import logging
import os
import sys
import time
from pathlib import Path

READY_FILE = "ready-for-checkpoint"
RESUME_FILE = "restore-complete"

log = logging.getLogger("snapshot-shim")


# --------------------------------------------------------------------------
# The protocol. Twenty lines; the rest of this file is vLLM plumbing.
# --------------------------------------------------------------------------

def announce_ready(quiesce_dir: Path) -> None:
    """Tell the operator this process is safe to dump."""
    quiesce_dir.mkdir(parents=True, exist_ok=True)
    resume = quiesce_dir / RESUME_FILE
    # A resume file left over from a previous life would end the wait
    # immediately and the checkpoint would catch the process mid-startup.
    if resume.exists():
        resume.unlink()
    (quiesce_dir / READY_FILE).touch()
    log.info("quiesce point reached: wrote %s", quiesce_dir / READY_FILE)


def wait_for_resume(quiesce_dir: Path, poll_seconds: float = 0.05) -> None:
    """Block until restored.

    Deliberately a plain synchronous loop: ``os.path.exists`` plus
    ``time.sleep`` is about the simplest thing CRIU can be asked to freeze.
    An async wait would park the process inside the event loop's epoll with a
    pending timer, which works but is more state than this needs.

    There is no timeout. The process is not serving and has nothing else to
    do; the operator bounds the wait from outside via quiesce-timeout.
    """
    resume = quiesce_dir / RESUME_FILE
    log.info("blocking for %s — the checkpoint happens here", resume)
    while not resume.exists():
        time.sleep(poll_seconds)
    log.info("resumed: %s appeared", resume)


# --------------------------------------------------------------------------
# vLLM adapter
# --------------------------------------------------------------------------

async def _warmup(engine) -> None:
    """Run one tiny generation so CUDA graphs and kernels are captured.

    Best-effort: vLLM already captures graphs during startup, so a failure
    here costs a little first-token latency after restore, not correctness.
    """
    try:
        from vllm.sampling_params import SamplingParams

        async for _ in engine.generate(
            "warmup", SamplingParams(max_tokens=1, temperature=0.0), "shim-warmup"
        ):
            pass
        log.info("warmup generation complete")
    except Exception as exc:  # noqa: BLE001 - genuinely best-effort
        log.warning("warmup generation failed (continuing): %s", exc)


async def _init_state(engine, vllm_config, state, args) -> None:
    """Call vLLM's init_app_state across the versions that changed it.

    0.8 took model_config; 0.9 takes the whole vllm_config. Rather than pin a
    version, ask the function what it wants.
    """
    from vllm.entrypoints.openai.api_server import init_app_state

    params = inspect.signature(init_app_state).parameters
    if "vllm_config" in params:
        await init_app_state(engine, vllm_config, state, args)
    else:
        await init_app_state(engine, vllm_config.model_config, state, args)


async def _serve_vllm(args, quiesce_dir: Path, sleep_level: int, warmup: bool) -> None:
    import uvicorn
    from vllm.entrypoints.openai.api_server import (
        build_app,
        build_async_engine_client,
    )

    async with build_async_engine_client(args) as engine:
        vllm_config = await engine.get_vllm_config()
        log.info("engine initialized")

        if warmup:
            await _warmup(engine)

        # Release the KV cache's physical pages while keeping the virtual
        # address reservation, so the CUDA graphs captured above stay valid
        # across the restore. level=1 also offloads weights to host RAM,
        # which is roughly neutral for us: cuda-checkpoint moves device
        # memory to the host during the dump anyway.
        if sleep_level > 0:
            await engine.sleep(level=sleep_level)
            log.info("engine asleep (level=%d); KV cache physical pages released", sleep_level)

        announce_ready(quiesce_dir)
        wait_for_resume(quiesce_dir)

        if sleep_level > 0:
            await engine.wake_up()
            log.info("engine awake; KV cache re-mapped")

        # Only now does anything listen on a socket.
        app = build_app(args)
        await _init_state(engine, vllm_config, app.state, args)

        config = uvicorn.Config(
            app,
            host=args.host or "0.0.0.0",  # noqa: S104 - a pod-local server
            port=args.port,
            log_level=args.uvicorn_log_level,
            # asyncio, not uvloop. uvloop's libuv may back its file
            # operations with io_uring, and CRIU cannot dump an io_uring
            # ring. The frontend is created after the quiesce point so it is
            # not in *this* image either way — but a restored process that is
            # snapshotted again must still be dumpable.
            loop="asyncio",
        )
        log.info("starting HTTP frontend on %s:%s", config.host, config.port)
        await uvicorn.Server(config).serve()


def _parse_vllm_args(argv: list[str]):
    from vllm.entrypoints.openai.cli_args import make_arg_parser
    from vllm.utils import FlexibleArgumentParser

    parser = make_arg_parser(FlexibleArgumentParser(description="vLLM under the snapshot shim"))
    args = parser.parse_args(argv)
    try:
        from vllm.entrypoints.openai.cli_args import validate_parsed_serve_args

        validate_parsed_serve_args(args)
    except ImportError:
        pass
    return args


def run_vllm(argv: list[str], quiesce_dir: Path, sleep_level: int, warmup: bool) -> None:
    import asyncio

    # Keep the engine in this process. vLLM's V1 engine otherwise runs
    # EngineCore in a child talking ZMQ over IPC sockets: CRIU can dump a
    # process tree, but every extra socket between the parts is another thing
    # that has to come back correctly.
    os.environ.setdefault("VLLM_ENABLE_V1_MULTIPROCESSING", "0")

    args = _parse_vllm_args(argv)
    if not getattr(args, "enable_sleep_mode", False) and sleep_level > 0:
        # sleep()/wake_up() are only available when the engine was built for
        # it — the allocator has to be the CUDA VMM one from the start.
        args.enable_sleep_mode = True
        log.info("forcing --enable-sleep-mode (required for sleep level %d)", sleep_level)

    asyncio.run(_serve_vllm(args, quiesce_dir, sleep_level, warmup))


# --------------------------------------------------------------------------
# Self-test: the protocol without a GPU, for validating the plumbing
# --------------------------------------------------------------------------

def run_self_test(quiesce_dir: Path) -> None:
    state = {"counter": 0}
    for _ in range(5):
        state["counter"] += 1
        time.sleep(0.1)
    log.info("pre-checkpoint counter=%d", state["counter"])

    announce_ready(quiesce_dir)
    wait_for_resume(quiesce_dir)

    # A restored process continues here with its state intact; the counter
    # carrying on from 5 is the proof.
    while True:
        state["counter"] += 1
        log.info("post-restore counter=%d", state["counter"])
        time.sleep(5)


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
        allow_abbrev=False,
    )
    parser.add_argument(
        "--quiesce-dir",
        default=os.environ.get("SNAPSHOT_QUIESCE_DIR", "/snapshot"),
        help="Rendezvous directory; must match podsnapshot.io/quiesce-dir (default: %(default)s).",
    )
    parser.add_argument(
        "--sleep-level",
        type=int,
        default=1,
        choices=(0, 1, 2),
        help="vLLM sleep level before the checkpoint. 0 disables KV release "
        "(dumps the full device allocation). Default: %(default)s.",
    )
    parser.add_argument(
        "--no-warmup",
        action="store_true",
        help="Skip the warmup generation before quiescing.",
    )
    parser.add_argument(
        "--self-test",
        action="store_true",
        help="Run the protocol with a trivial counter workload instead of vLLM.",
    )
    parser.add_argument(
        "vllm_args",
        nargs=argparse.REMAINDER,
        help="Arguments for `vllm serve`, after a bare --.",
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s [snapshot-shim] %(message)s",
        stream=sys.stdout,
    )

    quiesce_dir = Path(args.quiesce_dir)

    if args.self_test:
        run_self_test(quiesce_dir)
        return 0

    vllm_args = args.vllm_args
    if vllm_args and vllm_args[0] == "--":
        vllm_args = vllm_args[1:]
    if not vllm_args:
        parser.error("no workload given: pass `vllm serve` arguments after -- , or use --self-test")

    run_vllm(vllm_args, quiesce_dir, args.sleep_level, not args.no_warmup)
    return 0


if __name__ == "__main__":
    sys.exit(main())
