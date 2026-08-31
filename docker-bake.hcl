variable "IMAGE_REPOSITORY" {
  default = "ghcr.io/janpuc/t3-code-operator"
}

variable "STABLE_TAG" {
  default = "stable"
}

variable "NIGHTLY_TAG" {
  default = "nightly"
}

group "default" {
  targets = ["stable"]
}

target "runtime" {
  context    = "."
  dockerfile = "images/runtime/Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  attest = [
    "type=provenance,mode=max",
    "type=sbom"
  ]
}

target "stable" {
  inherits = ["runtime"]
  tags     = ["${IMAGE_REPOSITORY}:${STABLE_TAG}"]
  args = {
    T3_CHANNEL = "stable"
    T3_VERSION = "0.0.34"
  }
}

target "nightly" {
  inherits = ["runtime"]
  tags     = ["${IMAGE_REPOSITORY}:${NIGHTLY_TAG}"]
  args = {
    T3_CHANNEL = "nightly"
    T3_VERSION = "0.0.36-nightly.20260828.1209"
  }
}
