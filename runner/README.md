# ReleaseDock Release Runner

`releasedock-runner` is the secret-bearing runtime component of ReleaseDock. It
claims PostgreSQL jobs, stages and verifies an artifact, safely extracts it,
imports and pushes container images to Harbor, executes only administrator-
approved script versions through a separate executor UID, checks the deployment,
and persists every state and output chunk.

The portal/API must not mount a container-runtime socket or hold target-system
credentials. Run this process on a separately controlled runner host.

## Build and start

```bash
cd runner
go test ./...
go build -o releasedock-runner ./cmd/runner
go build -o releasedock-executor ./cmd/executor

POSTGRES_DSN='postgres://releasedock:...@postgres/releasedock?sslmode=verify-full' \
ENCRYPTION_KEY='base64-encoded-32-byte-key' \
./releasedock-runner
```

Production start uses the packaged systemd socket/service units. The executor
requires the root-owned socket-activation descriptor at fd 3 and intentionally
cannot be started as root or as the Runner user.

Those are the only two environment variables read by the process:

- `POSTGRES_DSN` connects to the shared ReleaseDock database.
- `ENCRYPTION_KEY` is a base64 or hex encoding of exactly 32 bytes and decrypts
  approved credentials with AES-256-GCM.

Poll timing, heartbeat timing, workspace path, restricted command `PATH`, and
log limits come from `runner_settings`. The artifact path comes
from `app_settings(id='default').artifact_storage_path`, which is also managed
by the API's storage settings UI. Extraction, runtime, Harbor, health, script,
timeout and cleanup policies come from the selected immutable deployment
profile. No operational setting has an environment-variable fallback.

Build metadata may be embedded without adding runtime configuration:

```bash
go build -trimpath \
  -ldflags "-s -w -X main.version=1.0.0 -X main.commit=<commit> -X main.builtAt=<UTC-time>" \
  -o releasedock-runner ./cmd/runner
```

`./releasedock-runner -version` prints that metadata.

## Database contract

The backend binary embeds and applies the production migrations before the
Runner starts. [`schema.sql`](schema.sql) is a development/reference snapshot;
do not apply it separately to an installed ReleaseDock database. The contract
uses backend-compatible UUID domain IDs (users/roles and runner worker IDs remain
text) and these existing names:

- `release_jobs.attempts`, `locked_by`, `locked_at`, `available_at`
- `deployment_profiles.active`
- `app_settings.id='default'` and `artifact_storage_path`
- `releases.id`, `status`, `updated_at`

The migration adds typed runner profile columns and the queue metadata required
by the worker. It also creates settings, credential versions, approved script
versions, profile-script bindings, release locks, steps, streaming logs and
image-digest records.

Before a job enters `QUEUED`, the API must populate its `application`,
`version`, `environment`, `lock_key`, artifact path/SHA256, `profile_id`, and an
immutable snapshot of the profile's required `runner_labels`. DEPLOY jobs also
freeze the prior verified `deployment_heads.current_release_id` in
`rollback_source_release_id`; it is empty only for the first deployment.
`release_jobs.status` must allow:

```text
QUEUED → VALIDATING → PRE_CHECK (when configured) → EXTRACTING → IMAGE_INSPECT → IMAGE_LOAD
       → IMAGE_TAG → IMAGE_PUSH → DEPLOYING → VERIFYING → SUCCESS
          any non-terminal step ───────────────────────────→ FAILED

manual ROLLBACK → VALIDATING(source image digests) → ROLLBACK → VERIFYING → ROLLED_BACK
```

The claim transaction uses this concurrency primitive:

```sql
SELECT ...
FROM release_jobs j
JOIN runner_instances ri
  ON ri.worker_id = $runner_identity
 AND ri.active
 AND ri.last_heartbeat_at >= clock_timestamp() - interval '60 seconds'
WHERE j.status = 'QUEUED' AND j.available_at <= clock_timestamp()
  AND j.runner_labels <@ ri.labels
ORDER BY j.priority DESC, j.created_at ASC
FOR UPDATE OF j SKIP LOCKED
LIMIT 1;
```

Required labels use subset matching, so a `PROD` job cannot be claimed by a
runner that only advertises `DEV`. A mismatched job remains `QUEUED` and does
not block matching jobs behind it. The API also refuses to enqueue when no
active, recently heartbeating runner satisfies the profile snapshot.

It then inserts the job's administrator-defined `lock_key` into
`release_locks`. A common key is `<application-id>:<environment-id>`, preventing
two runners from deploying the same application/environment concurrently.
Heartbeats update both the job lease and release lock. An expired lease is
marked `FAILED` and unlocked, never replayed automatically: a runner can die
after reaching a target, so blind replay can duplicate a deployment. The
administrator can inspect logs and explicitly retry.

`FinishJob` updates `release_jobs` and the parent `releases.status` in the same
transaction and releases the deployment lock.

## Package contract

```text
manifest.yaml
images/
  api.tar
  web.tar
config/
  application-prod.yaml
README.md
```

Example `manifest.yaml`:

```yaml
application: crm
version: 2.4.1
images:
  - file: images/api.tar
    source: crm-api:2.4.1       # optional when present in image manifest.json
    repository: crm/api
    tag: 2.4.1
```

The parser rejects unknown YAML fields. If `images` is omitted, regular `.tar`
files below `images/` are discovered. Docker `manifest.json` and OCI
`index.json` are inspected without extracting image layers. The job application
and version must exactly match the manifest.

The outer package may be tar or gzip-compressed tar. Extraction enforces the
profile's compressed size, total extracted size, entry count and image count.
Absolute paths, `..` traversal, device/FIFO/hard-link entries and symlink parent
pivots are rejected. Symlinks are disabled by default; enabling them permits
only relative in-workspace targets and they can never be used as extraction
parents. Artifact symlinks are always rejected. SHA256 is computed while the
artifact is copied into its setgid+sticky `03770` job workspace, shared only
with the unprivileged executor UID. The artifact remains Runner-owned and
group-read-only, while the sticky directory prevents the executor from
replacing it between validation and extraction.

## Runtime and Harbor

Profiles select `docker`, `podman`, or `containerd`. The executable is restricted
to the exact kind-specific name (`docker`, `podman`, or `ctr`) below `/usr/bin`,
`/usr/local/bin`, `/usr/sbin`, or `/usr/local/sbin`; its directory and resolved
binary must be root-owned, executable, and not group/world-writable. Commands
use `exec.CommandContext` with separated arguments; no command
is built with `sh -c`. Every command has a timeout, runs in its job workspace,
gets a small explicit environment, and has its process group killed on timeout.
Docker/Podman passwords use `--password-stdin`. Podman receives a job-local
certificate directory and TLS verification flag. Containerd receives a temporary
mode-`0600` `hosts.toml` with CA/skip-verify policy. Registry auth, CA, and hosts
files live below the tmpfs-backed `/run/releasedock-credentials` systemd
RuntimeDirectory and are deleted even when a failed workspace is retained.
Docker TLS is daemon-managed: install the
CA/insecure-registry policy on the Docker host and the CA in the Runner OS trust
store; per-profile Docker TLS flags are rejected instead of being silently ignored.

After each push, the runner performs a Registry V2 `HEAD` and stores the exact
`Docker-Content-Digest` (`sha256:...`) in `release_images`. Registry endpoint,
project, optional CA PEM, optional insecure-TLS policy and encrypted credential
version are all profile/database values.

Credential rows are immutable versions. `ciphertext` has this format:

```text
v1:<base64-raw(nonce || AES-256-GCM-ciphertext)>
```

The additional authenticated data is `credential:<id>:v<version>`, and the
plaintext JSON is `{"username":"...","password":"..."}`. Only rows with
`approved_at IS NOT NULL` and `revoked_at IS NULL` are loaded. Rotation creates
a new credential row/version and a new immutable deployment-profile version;
revocation prevents future claims from loading the old secret.

## Approved scripts

Uploaded package scripts are never executed. A profile can bind ordered
`PRE_DEPLOY`, `DEPLOY`, `POST_DEPLOY`, and `ROLLBACK` entries to immutable
`script_versions`. At claim time the query requires an administrator approval
and no revocation. Immediately before execution, the runner recomputes SHA256,
writes a generated filename into the private workspace, invokes the approved
absolute interpreter with each argument separately, and removes the file. A
`PRE_DEPLOY` binding appears externally as the `PRE_CHECK` job step and runs
immediately after artifact SHA256 validation, before extraction or image runtime
operations.

Approved scripts never run in `releasedock-runner`. An `Isolated` command crosses
the fixed `/run/releasedock-executor/executor.sock`; the server accepts only the
configured Runner UID and the Runner accepts only the root-owned systemd listener.
Linux records the listener credential at `listen(2)`, before systemd passes fd 3
to the executor, so the client intentionally pins the listener peer to UID 0.
The executor has no EnvironmentFile, DB/key material, Registry credential or
container-runtime group. Its systemd namespace hides `/etc/releasedock`, runtime
sockets and other users' `/proc` entries.

Each executor process accepts exactly one request, writes its terminal response,
waits for the client close, and exits. systemd `KillMode=control-group` then removes background,
double-forked, and `setsid` descendants before a queued socket connection can
activate the next process. The socket path is
`root:releasedock-executor-client`; only Runner belongs to this dedicated group,
so the API and an approved script cannot enqueue their own requests.

Scripts receive only:

```text
PATH HOME LANG LC_ALL
RELEASEDOCK_JOB_ID RELEASEDOCK_RELEASE_ID RELEASEDOCK_APPLICATION
RELEASEDOCK_VERSION RELEASEDOCK_ENVIRONMENT
RELEASEDOCK_ARTIFACT RELEASEDOCK_PACKAGE_DIRECTORY RELEASEDOCK_IMAGES
RELEASEDOCK_OPERATION RELEASEDOCK_ROLLBACK_SOURCE_RELEASE_ID
RELEASEDOCK_ROLLBACK_SOURCE_JOB_ID
RELEASEDOCK_CREDENTIAL_TYPE RELEASEDOCK_CREDENTIAL_FILE
```

Manual rollback does not re-stage/extract an artifact or load, tag, and push an
image. Claiming the job loads images only from its frozen
`rollback_source_job_id`, which must be the successful DEPLOY basis for the
frozen source release; mutable names, versions, and profile labels do not select
the source. Every stored destination and digest must be complete. The validation
step checks that each digest still exists in the configured Registry and passes
digest-pinned image references to the approved ROLLBACK script.
Artifact/package variables are empty for this path, followed by the profile
health checks.

`FinishJob` changes `deployment_heads` in the same transaction as a verified
DEPLOY `SUCCESS` or manual ROLLBACK `ROLLED_BACK`. The head's `current_job_id`
is the successful DEPLOY job that supplied the current image set. FAILED jobs
and stale-job recovery never alter it, so sequential rollback follows the
frozen A→B→C history instead of release creation order.

Registry secrets are not exposed to scripts. An optional profile-bound target
credential is decrypted by Runner immediately before each approved script into
a Runner-owned `0640` handoff below the non-listable, tmpfs-backed
`/run/releasedock-credentials` RuntimeDirectory. The executor validates that
file, copies it to an executor-owned `0600` file in its private per-activation
RuntimeDirectory (compatible with SSH StrictModes), replaces only the path in
the command environment, and unlinks the copy before responding. systemd clears
both RuntimeDirectories after crash/restart or power cycle, and both processes
sweep strictly validated managed paths at startup. Runner also removes its
handoff on every return path. Exact plaintext echoed
across stdout/stderr chunk boundaries is redacted before bounded chunks enter
`release_job_logs`. Approved scripts remain trusted deployment code: they can
transform or transmit a credential they are authorized to use.

Runner holds a non-blocking kernel `flock` in its private RuntimeDirectory for
its full lifetime. A second Runner process on the same host is rejected, keeping
the execute-only handoff tree and one-request executor strictly single-job.

Automatic rollback is deliberately disabled in v0.1: a failed deploy does not
have a safely frozen previous deployment head, so replaying a ROLLBACK script
with the current job context would be ambiguous. Profiles with
`auto_rollback=true` are rejected. Use the explicit rollback path above, which
freezes and validates the previous successful image digests and runs health
checks. If approval workflow is enabled, the API must create/transition a job
to `QUEUED` only after approval. With approval disabled, the API may queue
directly; the runner intentionally has no approval bypass.

## Health checks

`deployment_profiles.health_checks` is a JSON array. HTTP(S) checks allow GET or
HEAD, an expected status range, optional response substring, headers, retries,
custom CA and an explicit insecure-TLS option. TCP checks open the configured
address. Example:

```json
[
  {
    "type": "http",
    "address": "https://crm.internal/health",
    "method": "GET",
    "expectedStatusMin": 200,
    "expectedStatusMax": 299,
    "expectedBody": "UP",
    "timeoutSeconds": 5,
    "attempts": 12,
    "intervalSeconds": 5,
    "caPem": "-----BEGIN CERTIFICATE-----..."
  },
  {
    "type": "tcp",
    "address": "crm.internal:8443",
    "timeoutSeconds": 3,
    "attempts": 3,
    "intervalSeconds": 2
  }
]
```

## Offline operation

The module has no SaaS dependency. Build dependencies must be vendored or
downloaded before entering the offline network; the release artifact contains
the three compiled Linux binaries, embedded backend migrations and installation
documentation. At
runtime it talks only to configured PostgreSQL, Harbor, health endpoints and
the local container runtime/target tools invoked by approved scripts.
