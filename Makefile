# midea-mcp-control build helpers.
#
# The container image is published to Docker Hub and pulled by the showboat
# stack (deployments/midea/docker-compose.yaml there). The LAN inventory never
# enters the image — it is injected at runtime as a Pulumi secret.
#
#   make docker-build   # build mheers/midea-mcp-control:<version>
#   make docker-push    # build and push it
#
# Override the tag with TAG=...; the default comes from
# internal/version/version.go so the image and the binary's --version agree.

IMAGE   ?= mheers/midea-mcp-control
VERSION := $(shell sed -n 's/^const Value = "\(.*\)"/\1/p' internal/version/version.go)
TAG     ?= $(VERSION)

.PHONY: test vet race build docker-build docker-push

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

build:
	CGO_ENABLED=0 go build -trimpath -o bin/midea-mcp-control ./cmd/midea-mcp-control

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push: docker-build
	docker push $(IMAGE):$(TAG)
	@echo "pushed $(IMAGE):$(TAG)"
