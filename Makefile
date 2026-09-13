# PachiCounter2 のビルドと開発作業。
#
# cgo は使わないので、クロスコンパイルは GOOS/GOARCH を渡すだけで済む。
# フロントはビルド無しでバイナリに埋め込むので、ここに npm は出てこない
# （docs/adr/0007-buildless-embedded-front-with-override.md を参照）。

BIN      := pachicounter
CMD      := ./cmd/pachicounter
DIST_DIR := dist

GO      ?= go
GOFLAGS ?=
LDFLAGS ?= -s -w

# 配布用にクロスコンパイルする対象。GOOS と GOARCH を - で繋いだ形で並べる。
PLATFORMS := linux-amd64 linux-arm64 linux-arm windows-amd64

# Raspberry Pi の 32bit OS 向け。GOARCH=arm のときだけ効く。
GOARM ?= 7

# go run に渡す引数。例: make run ARGS='-source loop:session.jsonl'
ARGS ?=

export CGO_ENABLED := 0

.DEFAULT_GOAL := build


# --- ビルド --------------------------------------------------------------

.PHONY: build
build: ## 手元の環境向けにビルドする
	$(GO) build $(GOFLAGS) -trimpath -o $(BIN) $(CMD)

.PHONY: install
install: ## $(GOBIN) へ入れる
	$(GO) install $(GOFLAGS) -trimpath $(CMD)

.PHONY: run
run: ## ビルドせずに動かす（例: make run ARGS='-source loop:session.jsonl'）
	$(GO) run $(CMD) $(ARGS)

.PHONY: dist
dist: $(addprefix dist-,$(PLATFORMS)) ## 配布用に全対象をクロスコンパイルする

.PHONY: dist-%
dist-%: ## 指定した GOOS-GOARCH 向けにビルドする（例: make dist-linux-arm64）
	@os=$(word 1,$(subst -, ,$*)); \
	arch=$(word 2,$(subst -, ,$*)); \
	ext=""; \
	if [ "$$os" = "windows" ]; then \
		ext=".exe"; \
	fi; \
	out="$(DIST_DIR)/$(BIN)-$$os-$$arch$$ext"; \
	mkdir -p $(DIST_DIR); \
	echo "build $$out"; \
	GOOS=$$os GOARCH=$$arch GOARM=$(GOARM) \
		$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o "$$out" $(CMD)


# --- 検査 ----------------------------------------------------------------

.PHONY: check
check: fmt-check vet test ## 書式・vet・テストをまとめて通す

.PHONY: test
test: ## テストを走らせる
	$(GO) test ./...

.PHONY: test-race
test-race: ## データ競合の検出付きでテストを走らせる（cgo が要る）
	CGO_ENABLED=1 $(GO) test -race ./...

.PHONY: cover
cover: ## カバレッジを測って HTML で開く
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out

.PHONY: vet
vet: ## go vet をかける
	$(GO) vet ./...

.PHONY: fmt
fmt: ## gofmt で整形する
	gofmt -w -s .

.PHONY: fmt-check
fmt-check: ## 整形されていないファイルがないか見る
	@out=$$(gofmt -l -s .); \
	if [ -n "$$out" ]; then \
		echo "gofmt をかけていないファイルがある:"; \
		echo "$$out"; \
		exit 1; \
	fi

.PHONY: lint
lint: ## golangci-lint をかける（入っていれば）
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint が無い: https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	fi

.PHONY: tidy
tidy: ## go.mod と go.sum を整える
	$(GO) mod tidy


# --- 環境 ----------------------------------------------------------------

.PHONY: install-udev
install-udev: ## udev ルールを入れて /dev/hidraw を読めるようにする（要 sudo・Linux のみ）
	@# 以前の名前（99-）で入れたものは uaccess が効かないので消す
	sudo rm -f /etc/udev/rules.d/99-pachicounter.rules
	sudo cp contrib/udev/60-pachicounter.rules /etc/udev/rules.d/
	sudo udevadm control --reload-rules
	sudo udevadm trigger

.PHONY: clean
clean: ## ビルド成果物を消す
	rm -f $(BIN) $(BIN).exe coverage.out
	rm -rf $(DIST_DIR)

.PHONY: help
help: ## この一覧を出す
	@grep -hE '^[a-zA-Z0-9_%-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN { FS = ":.*?## " }; { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 }'
