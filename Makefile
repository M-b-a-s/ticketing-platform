-include .env
export

.PHONY: up down run-api run-worker build test vet lint tidy

up:
	docker compose up -d --wait

down:
	docker compose down

run-api:
	go run ./cmd/api

run-worker:
	go run ./cmd/worker

build:
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run

tidy:
	go mod tidy
