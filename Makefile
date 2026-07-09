.PHONY: build lint run

TZ := $(firstword $(filter-out run force,$(MAKECMDGOALS)))
FORCE := $(if $(filter force,$(MAKECMDGOALS)),--force)

build:
	go mod tidy
	go build -o ./main .

lint:
	golangci-lint run ./...

run:
	./main $(if $(TZ),--tz=$(TZARG)) $(FORCE)

%:
	@:
