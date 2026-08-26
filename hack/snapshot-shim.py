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
# Making the process dumpable
# --------------------------------------------------------------------------

def nvidia_fds() -> dict[str, int]:
    """Count this process's open /dev/nvidia* file descriptors, by device."""
    counts: dict[str, int] = {}
    fd_dir = Path("/proc/self/fd")
    try:
        entries = list(fd_dir.iterdir())
    except OSError:
        return counts
    for entry in entries:
        try:
            target = os.readlink(entry)
        except OSError:
            continue
        if target.startswith("/dev/nvidia"):
            counts[target] = counts.get(target, 0) + 1
    return counts


def _import_pynvml():
    """Return whatever NVML binding this image ships, or None.

    The vLLM container has no top-level ``pynvml``: it vendors the binding at
    ``vllm.third_party.pynvml`` to avoid the well-known clash between the
    ``pynvml`` and ``nvidia-ml-py`` distributions. Try both.
    """
    import importlib

    for name in ("pynvml", "vllm.third_party.pynvml"):
        try:
            return importlib.import_module(name)
        except ImportError:
            continue
    return None


def release_nvml() -> str:
    """Close NVML's handle on /dev/nvidiactl.

    ``cuda-checkpoint`` hands back every descriptor the CUDA *runtime* owns,
    which is what lets CRIU dump the process at all. NVML is a different
    library with its own /dev/nvidiactl fd, and nothing in the checkpoint path
    knows about it — so it survives into the dump and CRIU stops with::

        Error (criu/files-ext.c:94): Can't dump file 9 of that type
        [20666] (chr 195:255)

    195:255 is /dev/nvidiactl. Measured on vLLM 0.9.2 + CRIU 4.2.1: the engine
    worker held two nvidiactl fds and the dump failed on the second.

    nvmlInit/nvmlShutdown are reference counted and several libraries
    (PyTorch's device-count probe, vLLM's platform checks) call Init without a
    matching Shutdown, so unwind until it complains.
    """
    pynvml = _import_pynvml()
    if pynvml is None:
        return "no NVML binding importable"

    calls = 0
    for _ in range(64):
        try:
            pynvml.nvmlShutdown()
        except Exception:  # noqa: BLE001 - NVMLError_Uninitialized ends the unwind
            break
        calls += 1
    return f"nvmlShutdown x{calls}"


def orphan_deleted_shm() -> str:
    """Unlink the surviving names of any deleted-but-mapped /dev/shm file.

    glibc's sem_open creates /dev/shm/sem.XXXXXX, mmaps it, hard-links it to
    the caller's name, and unlinks the temp — so the mapping permanently
    refers to a deleted path even though the inode is alive under another
    name. Any Python multiprocessing Lock/Queue/Event leaves one behind, and
    CRIU cannot ghost a *mapped* deleted file while it still has a link:

        Error (criu/files-reg.c:1122): Can't create link remap for
        /dev/shm/sem.XXXXXX. Use link-remap option.

    link-remap is not a way out. It hard-links the inode to
    /dev/shm/link_remap.N during the dump, drops that link when the dump
    finishes, and expects it back at restore time. A restored pod gets an
    empty tmpfs, so the restore dies with:

        Error (criu/files-reg.c:2258): Can't link dev/shm/link_remap.337 ->
        dev/shm/sem.l0JKK0: No such file or directory

    Dropping the last link instead takes nlink to zero, which is exactly the
    case CRIU handles by writing a ghost file — content and all — into the
    image. Ghosts restore anywhere, which is what we need.

    Unlinking a POSIX semaphore that is still mapped is well defined: the
    inode lives until the last mapping goes, and by this point nothing will
    look it up by name again.

    /proc/self/maps carries the inode, so this needs no privileges.
    """
    import re

    try:
        lines = Path("/proc/self/maps").read_text().splitlines()
    except OSError as exc:
        return f"could not read /proc/self/maps: {exc}"

    # address perms offset dev inode pathname
    pattern = re.compile(r"^\S+ \S+ \S+ \S+ (\d+) +(/dev/shm/\S+) \(deleted\)$")
    orphaned = {int(m.group(1)) for m in map(pattern.match, lines) if m and int(m.group(1))}
    if not orphaned:
        return "no deleted /dev/shm mappings"

    removed = []
    try:
        entries = list(Path("/dev/shm").iterdir())
    except OSError as exc:
        return f"could not list /dev/shm: {exc}"
    for entry in entries:
        try:
            if entry.stat().st_ino not in orphaned:
                continue
            entry.unlink()
        except OSError:
            continue
        removed.append(entry.name)
    return f"orphaned {len(orphaned)} deleted /dev/shm mapping(s); unlinked {removed or 'nothing'}"


def _worker_make_dumpable(worker) -> str:  # noqa: ANN001 - vLLM passes its Worker
    """collective_rpc payload: the same cleanup, inside the engine worker.

    The worker is where the model lives, and in vLLM's V1 engine it is a
    different process from the one running this shim — so the frontend
    cleaning up after itself is not enough.
    """
    import importlib
    import os
    import re
    from pathlib import Path

    def _fds():
        out = {}
        for entry in Path("/proc/self/fd").iterdir():
            try:
                target = os.readlink(entry)
            except OSError:
                continue
            if target.startswith("/dev/nvidia"):
                out[target] = out.get(target, 0) + 1
        return out

    before = _fds()
    calls = 0
    for name in ("pynvml", "vllm.third_party.pynvml"):
        try:
            pynvml = importlib.import_module(name)
        except ImportError:
            continue
        for _ in range(64):
            try:
                pynvml.nvmlShutdown()
            except Exception:  # noqa: BLE001
                break
            calls += 1
    # Same /dev/shm orphaning as the frontend does; see orphan_deleted_shm.
    pattern = re.compile(r"^\S+ \S+ \S+ \S+ (\d+) +(/dev/shm/\S+) \(deleted\)$")
    orphaned = set()
    for line in Path("/proc/self/maps").read_text().splitlines():
        m = pattern.match(line)
        if m and int(m.group(1)):
            orphaned.add(int(m.group(1)))
    removed = []
    for entry in Path("/dev/shm").iterdir():
        try:
            if entry.stat().st_ino not in orphaned:
                continue
            entry.unlink()
        except OSError:
            continue
        removed.append(entry.name)

    return (f"pid={os.getpid()} nvmlShutdown x{calls} fds {before} -> {_fds()} "
            f"shm orphaned={len(orphaned)} unlinked={removed or 'nothing'}")


async def make_dumpable(engine) -> None:
    """Drop everything CRIU cannot dump, in this process and in the workers."""
    log.info("nvidia fds before cleanup: %s", nvidia_fds())
    log.info("frontend: %s; %s", release_nvml(), orphan_deleted_shm())
    try:
        results = await engine.collective_rpc(_worker_make_dumpable)
        for r in results or ():
            log.info("worker: %s", r)
    except Exception as exc:  # noqa: BLE001 - a single-process engine has no workers to call
        log.warning("could not run the dumpability cleanup in the engine workers: %s", exc)
    log.info("nvidia fds after cleanup: %s", nvidia_fds())


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

        await make_dumpable(engine)
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

    # Spawn the engine process instead of forking it. This one matters.
    #
    # vLLM's V1 async path always runs EngineCore in its own process
    # (asyncio + in-process EngineCore is explicitly NotImplemented), and
    # vLLM defaults to fork. A forked EngineCore inherits every descriptor
    # the frontend had open — including the /dev/nvidiactl that NVML opened
    # while probing the device. That inherited descriptor belongs to no
    # library in the child: cuda-checkpoint does not own it, so it is still
    # open when CRIU walks the process, and the dump fails with
    #
    #   Error (criu/files-ext.c:94): Can't dump file 9 of that type
    #   [20666] (chr 195:255)
    #
    # spawn re-execs, so the engine starts with a clean descriptor table and
    # the only nvidia fds it has are ones cuda-checkpoint hands back.
    # (vLLM already forces spawn when the parent has initialized CUDA — but
    # NVML alone does not count as initialized, which is the gap.)
    os.environ.setdefault("VLLM_WORKER_MULTIPROC_METHOD", "spawn")

    # Bind vLLM's internal sockets to loopback rather than the pod IP.
    #
    # The engine's ZMQ endpoints and the torch.distributed TCPStore are
    # created during engine init, before the quiesce point, so they ARE in
    # the image however late the frontend starts. CRIU restores a bound
    # socket by binding the same address again — and the restored pod has a
    # different IP, so it fails:
    #
    #   Error (soccr/soccr.c:499): Can't bind inet socket back:
    #   Cannot assign requested address
    #
    # Loopback exists identically in every pod, so these rebind anywhere.
    # This is single-node only, which is what the quiesce path supports
    # today; multi-node engines need the distributed runtime rebuilt on the
    # resume side instead (docs/design-v2.md §4).
    os.environ.setdefault("VLLM_HOST_IP", "127.0.0.1")

    # Same reason, different library. torch.distributed's gloo backend picks
    # its interface itself and ignores VLLM_HOST_IP, so it binds a handful of
    # listeners to the pod IP even for a single-GPU engine. Point it at
    # loopback.
    os.environ.setdefault("GLOO_SOCKET_IFNAME", "lo")
    os.environ.setdefault("TP_SOCKET_IFNAME", "lo")

    # No outbound connections at the quiesce point. vLLM's usage reporting
    # keeps an established TLS connection to a host on the public internet,
    # and CRIU has no way to bring that back on another node.
    os.environ.setdefault("VLLM_NO_USAGE_STATS", "1")
    os.environ.setdefault("DO_NOT_TRACK", "1")
    # collective_rpc refuses to ship a callable unless this is set. The
    # channel is vLLM's own engine IPC inside this pod, and the callable is
    # the NVML cleanup below, so the "insecure" it is guarding against does
    # not apply here.
    os.environ.setdefault("VLLM_ALLOW_INSECURE_SERIALIZATION", "1")

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
