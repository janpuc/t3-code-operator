GO ?= go

.PHONY: chart-crds fmt generate manifests test test-api test-chart test-image test-kind

fmt:
	$(GO) fmt ./...

generate:
	$(GO) tool controller-gen object paths=./...

manifests:
	$(GO) tool controller-gen crd:maxDescLen=0 paths=./api/... output:crd:artifacts:config=config/crd/bases

chart-crds: manifests
	cp config/crd/bases/*.yaml charts/t3-code-operator/crds/

test:
	$(GO) test ./...

test-api:
	T3_PHASE1_ENVTEST=1 $(GO) test ./api/v1alpha1 -run TestAPIServer

test-chart: chart-crds
	bash ./hack/phase5/chart-contract.sh

test-image:
	$(GO) test ./images/runtime

test-kind:
	bash ./hack/phase1/kind-api-contract.sh
