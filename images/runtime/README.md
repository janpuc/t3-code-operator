# Runtime image

This image contains upstream t3-code, `t3-coded`, the operator manager, all five upstream provider executables, `mise`, and a fixed tool baseline.

It also contains `t3-smbd` and Samba administration tools. The operator starts them only when a Workstation enables SMB workspace sharing.

The image supports Linux amd64 and arm64. Cursor and Grok remain alpha because authenticated tests are unavailable.

## Tracks

- `stable` contains `t3@0.0.34`.
- `nightly` contains the exact nightly version in `npm/nightly/package.json`.

Both tracks use npm lockfiles. The Dockerfile also pins its base images, `mise`, Cursor, and Grok by SHA-256 digest.

Build the stable track with:

```sh
docker buildx bake stable
```

Build the nightly track with:

```sh
docker buildx bake nightly
```

The build runs each CLI version check. It loads the required native Node modules and starts the selected upstream t3 package.

The build also starts `t3-smbd`, reaches its SMB socket, and verifies the generated local SID through Samba.

The image uses upstream executable contracts directly. It does not implement the container wrapper configuration model.

Publish OCI provenance and an SBOM. Deploy Workstation images only by digest.
