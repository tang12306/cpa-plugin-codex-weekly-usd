PLUGIN  := codex-weekly-usd
REPO    ?= github.com/tang12306/cpa-plugin-codex-weekly-usd
VERSION ?=
CC      ?= cc

# The plugin ABI is a plain C ABI over JSON, so cgo is required but the host's
# Go version is not: a plugin built with any toolchain loads into any CPA.
LDFLAGS := -s -w -X main.repository=$(REPO)
ifneq ($(VERSION),)
LDFLAGS += -X main.pluginVersion=$(VERSION)
endif

.PHONY: all build test test-accounting test-charts vet clean

all: build

build:
	CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags "$(LDFLAGS)" -o $(PLUGIN).so .
	@rm -f $(PLUGIN).h

vet:
	go vet ./...

build/harness: test/harness.c
	@mkdir -p build
	$(CC) -O1 -o $@ $< -ldl

# run_tests.py writes build/panel.html from the panel the plugin actually
# serves, and test_charts.js then asserts against those exact bytes.
test: test-accounting test-charts

test-accounting: build build/harness
	python3 test/run_tests.py

test-charts: test-accounting
	node test/test_charts.js build/panel.html

clean:
	rm -rf build $(PLUGIN).so $(PLUGIN).h
