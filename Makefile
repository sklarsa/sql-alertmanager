IMAGE ?= sklarsa/sql-alertmanager
TAG = $(shell git describe --tags --always)

.PHONY: docker-build docker-push build lint

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push:
	docker push $(IMAGE):$(TAG)

build:
	go build -o main .

test:
	go test -coverprofile=coverage.out ./...

coverage:
	go tool cover -func=coverage.out

lint:
	golangci-lint run
