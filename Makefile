GOBIN ?= $(shell go env GOPATH)/bin
CONTROLLER_GEN ?= $(GOBIN)/controller-gen
IMG_MANAGER ?= pod-snapshotter/manager:latest
IMG_AGENT ?= pod-snapshotter/agent:latest

.PHONY: all build test vet manifests generate proto docker-build helm-template clean

all: build

build:
	go build -o bin/manager ./cmd/manager
	go build -o bin/agent ./cmd/agent

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

# Regenerate zz_generated.deepcopy.go after editing api/ types.
generate:
	$(CONTROLLER_GEN) object:headerFile="" paths="./api/..."

# Regenerate CRDs (config/crd/bases + chart crds/) and RBAC after editing
# api/ types or +kubebuilder:rbac markers.
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=pod-snapshotter-manager \
		paths="./api/...;./internal/controller/...;./internal/agent/..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/*.yaml charts/pod-snapshotter/crds/

# Regenerate internal/pb from proto/ (agent.proto is a copy of
# fuse-client/proto/agent.proto with our go_package).
proto:
	protoc --go_out=. --go_opt=paths=import --go_opt=module=pod-snapshotter \
		--go-grpc_out=. --go-grpc_opt=paths=import --go-grpc_opt=module=pod-snapshotter \
		-I proto proto/agent.proto

docker-build:
	docker build -f Dockerfile.manager -t $(IMG_MANAGER) .
	docker build -f Dockerfile.agent -t $(IMG_AGENT) .

helm-template:
	helm template pod-snapshotter charts/pod-snapshotter

install-tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

clean:
	rm -rf bin/
