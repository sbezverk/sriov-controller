REGISTRY_NAME = 192.168.80.240:4000/ligato
IMAGE_VERSION = latest

.PHONY: all liveness clean test

ifdef V
TESTARGS = -v -args -alsologtostderr -v 5
else
TESTARGS =
endif

all: sriov-controller

sriov-controller:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o ./bin/sriov-controller ./sriov-controller.go service-controller.go dpapi-controller.go

mac:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=darwin go build -a -ldflags '-extldflags "-static"' -o ./bin/sriov-controller.mac ./sriov-controller.go

container: sriov-controller
	docker build -t $(REGISTRY_NAME)/sriov-controller:$(IMAGE_VERSION) -f ./Dockerfile .

push: container
	docker push $(REGISTRY_NAME)/sriov-controller:$(IMAGE_VERSION)

clean:
	rm -rf bin

test:
	go test `go list ./... | grep -v 'vendor'` $(TESTARGS)
	go vet `go list ./... | grep -v vendor`
