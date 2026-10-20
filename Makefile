# Локальный прогон CI (полный CI — .github/workflows/ci.yml).
# Использование: make test / make plugins / make all

.PHONY: all vet fmt test conformance build plugins pipelines registry clean

all: vet fmt test build plugins pipelines registry

vet:
	go vet ./...

fmt:
	test -z "$$(gofmt -l ./internal ./cmd/tool)"

test:
	go test ./... -count=1 -run Test

conformance:
	go test ./internal/core/ -run 'TestPluginTest|TestExec' -v -count=1

build:
	go build -o tool ./cmd/tool
	go build -o fidespipe ./cmd/fidespipe

plugins: build
	for d in plugins/official/* plugins/community/*; do \
		if [ -f "$$d/plugin.yaml" ]; then \
			echo "== $$d =="; \
			./fidespipe plugin validate "$$d"; \
			./fidespipe plugin test "$$d"; \
		fi; \
	done

pipelines: build
	for f in examples/*.yaml; do \
		./fidespipe pipeline validate "$$f"; \
		./fidespipe pipeline lint "$$f"; \
		./fidespipe pipeline plan "$$f"; \
	done

registry: build
	./fidespipe registry validate --registry=registry.yaml --local-source=.

clean:
	rm -f tool fidespipe tool.exe fidespipe.exe
