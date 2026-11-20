IMG ?= gryvia/network-operator:latest

.PHONY: all
all: build

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: fmt vet
	go test ./... -coverprofile cover.out

.PHONY: build
build: fmt vet
	go build -o bin/manager main.go

.PHONY: run
run: fmt vet
	go run ./main.go

.PHONY: docker-build
docker-build:
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push:
	docker push ${IMG}

.PHONY: deploy
deploy:
	kubectl apply -f ../../../crds/gryvianetwork.yaml
	kubectl apply -f config/

.PHONY: undeploy
undeploy:
	kubectl delete -f config/
