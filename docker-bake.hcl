variable "OPERATOR_IMAGE_REPOSITORY" {
  default = "ghcr.io/janpuc/t3-code-operator"
}

variable "RUNTIME_IMAGE_REPOSITORY" {
  default = "ghcr.io/janpuc/t3-code-runtime"
}

variable "SMBD_IMAGE_REPOSITORY" {
  default = "ghcr.io/janpuc/t3-code-smbd"
}

variable "STABLE_TAG" {
  default = "stable"
}

variable "RELEASE_VERSION" {
  default = ""
}

variable "NIGHTLY_TAG" {
  default = "nightly"
}

group "default" {
  targets = ["stable"]
}

group "release-digests" {
  targets = ["stable-digest", "operator-digest", "smbd-digest"]
}

target "base" {
  context    = "."
  dockerfile = "images/runtime/Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  attest = [
    "type=provenance,mode=max",
    "type=sbom"
  ]
}

target "runtime" {
  inherits = ["base"]
  target   = "runtime"
}

target "stable" {
  inherits = ["runtime"]
  tags = RELEASE_VERSION != "" ? [
    "${RUNTIME_IMAGE_REPOSITORY}:stable",
    "${RUNTIME_IMAGE_REPOSITORY}:v${RELEASE_VERSION}",
    "${RUNTIME_IMAGE_REPOSITORY}:${RELEASE_VERSION}",
  ] : ["${RUNTIME_IMAGE_REPOSITORY}:${STABLE_TAG}"]
  args = {
    T3_CHANNEL = "stable"
    T3_VERSION = "0.0.34"
  }
}

target "nightly" {
  inherits = ["runtime"]
  tags     = ["${RUNTIME_IMAGE_REPOSITORY}:${NIGHTLY_TAG}"]
  args = {
    T3_CHANNEL = "nightly"
    T3_VERSION = "0.0.36-nightly.20260828.1209"
  }
}

target "operator" {
  inherits = ["base"]
  target   = "operator"
  tags = RELEASE_VERSION != "" ? [
    "${OPERATOR_IMAGE_REPOSITORY}:latest",
    "${OPERATOR_IMAGE_REPOSITORY}:v${RELEASE_VERSION}",
    "${OPERATOR_IMAGE_REPOSITORY}:${RELEASE_VERSION}",
  ] : ["${OPERATOR_IMAGE_REPOSITORY}:latest"]
}

target "smbd" {
  inherits = ["base"]
  target   = "smbd"
  tags = RELEASE_VERSION != "" ? [
    "${SMBD_IMAGE_REPOSITORY}:latest",
    "${SMBD_IMAGE_REPOSITORY}:v${RELEASE_VERSION}",
    "${SMBD_IMAGE_REPOSITORY}:${RELEASE_VERSION}",
  ] : ["${SMBD_IMAGE_REPOSITORY}:latest"]
}

target "stable-digest" {
  inherits = ["stable"]
  tags     = []
  output   = ["type=image,name=${RUNTIME_IMAGE_REPOSITORY},push-by-digest=true,name-canonical=true,push=true"]
}

target "nightly-digest" {
  inherits = ["nightly"]
  tags     = []
  output   = ["type=image,name=${RUNTIME_IMAGE_REPOSITORY},push-by-digest=true,name-canonical=true,push=true"]
}

target "operator-digest" {
  inherits = ["operator"]
  tags     = []
  output   = ["type=image,name=${OPERATOR_IMAGE_REPOSITORY},push-by-digest=true,name-canonical=true,push=true"]
}

target "smbd-digest" {
  inherits = ["smbd"]
  tags     = []
  output   = ["type=image,name=${SMBD_IMAGE_REPOSITORY},push-by-digest=true,name-canonical=true,push=true"]
}
