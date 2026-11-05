.PHONY: test golden-update build vet

build:
	go build -o firmirror ./cmd/

vet:
	go vet ./...

test:
	go test -race ./...

golden-update:
	GO_GOLDEN_UPDATE=1 go test ./pkg/lvfs/ ./pkg/vendors/hpe/ ./pkg/vendors/dell/ -run TestGolden

golden-check:
	go test ./pkg/lvfs/ ./pkg/vendors/hpe/ ./pkg/vendors/dell/ -run TestGolden
