---
title: Releases and upgrades
weight: 7
---

## Cutting a release

Releases are tags named `version-<x.y.z>` (for example `version-0.3.3`). Pushing one runs the Release workflow, which:

1. Derives the version by stripping `version-`.
2. Builds static binaries for `linux/amd64` and `darwin/arm64` with `scripts/build-release.sh`, stamping the version into the binary, and writes `SHA256SUMS`.
3. Checks that the built binary reports the version.
4. Creates a GitHub release with the tarballs, checksums, and generated notes.

Use an **annotated** tag. Its annotation becomes the release's upgrade notes, so an operator-visible change, such as a new or newly enforced environment variable, travels with the tag:

```bash
git tag -a version-0.4.0 -m "Requires DAGNATS_LOOP_STATE_MAX_BYTES=524288 on the sidecar."
git push origin version-0.4.0
```

An unannotated tag still releases, with no upgrade notes. These docs read the latest `version-*` tag at build time to show the current version.

A binary built any other way reports `0.0.0-dev`, which makes a running instance unable to say what it is. Build with `scripts/build-release.sh <version> [os/arch …]`, which is also what the workflow runs.

## Upgrading

1. Read the upgrade notes of every release between yours and the target.
2. Back up the database ([Backups](../backups-and-retention)).
3. Run `notify validate-steps` with the **new** binary against the database. Some releases tighten workflow rules, rejecting forms that older versions accepted (for example a dynamic delay must now declare `dynamicKind`, and a scheduled digest's cron must be the 5-field form). This lists the affected workflows before you deploy rather than after.
4. Upgrade the dagnats sidecar if the new notify pins a newer one.
5. Replace the binary and restart. Schema migrations run at start. Workflows are re-registered with the engine automatically.
6. Check `/healthz` for the new version and `/readyz` for readiness.

The HTTP API follows an additive contract: within `Notify-Contract: 1`, routes and fields are added but not removed or reinterpreted.

## Upgrade note: verified phones for SMS (#130)

The release that adds [phone verification](../../inbox-and-agents/subscriber-sessions#verify-a-phone) **stops existing SMS workflows from sending** until you act:

- Every existing subscriber phone becomes unverified. An `sms` step texts only a phone the subscriber verified themselves, so until they do, every sms step is skipped with `skipReason: "no_verified_phone"`. Nothing fails; the other steps of the run still go out.
- SMS is off by default for a subscriber with no SMS preference. An existing sms step has no `defaultEnabled`, so it sends only to subscribers who turned SMS on for that workflow.

To restore SMS:

1. On each workflow that should text people without an opt-in (sign-off requests, for example), set `"defaultEnabled": true` on the sms step, or tick **Send when the subscriber has no SMS preference** in the console's step editor.
2. Have each subscriber verify their phone from your app: mint a [subscriber session](../../inbox-and-agents/subscriber-sessions) and call `POST /v1/me/phone/verification`, then `.../confirm` with the code they receive. There is no way to mark a phone verified from your backend; that is the point of the change.

Stored phones are also normalised to E.164 on their next write, so `+1 (555) 111-2222` is stored as `+15551112222`. A phone that is not a valid E.164 number is stored as written and can never be verified.
