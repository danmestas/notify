---
title: Blob storage
weight: 3
---

The blob store keeps immutable byte objects addressed by their SHA-256: today, the raw bytes of inbound mail, exactly as received. It exists so a message can be re-parsed, audited or handed back later.

It is **off by default**. Keeping every accepted email is a retention decision, so you make it explicitly with `NOTIFY_BLOB_DRIVER`. What gets stored, and the archive API, are in [Archive](../../inbound-email/archive).

## Properties

- **Content addressed.** An object's address is the lowercase hex SHA-256 of its bytes. The store computes the hash itself, so an object can never sit under the wrong address, and storing bytes that are already present does nothing.
- **Immutable.** There is no update and no delete in the interface.
- **Streaming.** Writes hash into a temporary file first, so large mail never sits in memory.
- Adapters are chosen by configuration only; nothing above the store knows which one is in use.

## fs

```bash
NOTIFY_BLOB_DRIVER=fs
NOTIFY_BLOB_DIR=/var/lib/notify/blobs     # default: <directory of NOTIFY_DB>/blobs
```

Objects are files fanned out by the first four hex characters (`ab/cd/abcd…`) so no directory grows without bound. A write goes to a temporary file in the root and is renamed into place, so a crash leaves at most a stray temporary file, never a partial object under a real address. This is the right choice for a single node. Back up the directory with the database.

## s3

```bash
NOTIFY_BLOB_DRIVER=s3
NOTIFY_BLOB_S3_ENDPOINT=127.0.0.1:3900     # host:port, no scheme
NOTIFY_BLOB_S3_REGION=garage
NOTIFY_BLOB_S3_BUCKET=notify-mail
NOTIFY_BLOB_S3_PREFIX=notify               # optional
NOTIFY_BLOB_S3_ACCESS_KEY=…
NOTIFY_BLOB_S3_SECRET_KEY=…
NOTIFY_BLOB_S3_TLS=false                   # true for HTTPS
```

Any store that speaks the S3 API works: [Garage](https://garagehq.deuxfleurs.fr/), SeaweedFS, MinIO, or a hosted S3. Objects are stored under `<prefix>/<ab>/<cd>/<sha256>`; the prefix lets several users share a bucket. Uploads are made with a known size, and an object already present is not uploaded again. The empty object is treated as always present without being uploaded, since S3 implementations differ on zero-length writes.

`NOTIFY_BLOB_S3_ENDPOINT` and `NOTIFY_BLOB_S3_BUCKET` are required. **notify does not create the bucket**; create it first.

### With Garage

Garage is a good fit for a self-hosted, single-node or small-cluster store. Roughly:

1. Run Garage and set up its layout, following its quick start.
2. Create a bucket and an access key, and allow the key to read and write the bucket:

   ```bash
   garage bucket create notify-mail
   garage key create notify
   garage bucket allow --read --write --owner notify-mail --key notify
   ```

3. Point notify at Garage's S3 API port (3900 by default) with the key it printed, and use the region from Garage's `s3_region` setting (`garage` is its default).

Garage listens on plain HTTP unless you terminate TLS in front of it, so leave `NOTIFY_BLOB_S3_TLS` off for a loopback or private-network endpoint, and turn it on if it is behind HTTPS. Check the Garage commands against your version's documentation.

## Failure behaviour

If the blob store is configured and a write fails while accepting inbound mail, the message is **not** accepted: the SMTP receiver answers `451` and the JSON webhook answers `500`, so the sender retries. Nothing is stored twice on the retry. If an archived object has gone missing from the store, `GET /v1/inbound/messages/{id}/raw` answers `404` with "the message's bytes are no longer stored".

## Backups

The database and the blob store are separate. A database backup contains the records (id, hash, sender, subject) but not the bytes. Back up the `fs` directory or the bucket on its own schedule. See [Backups and retention](../backups-and-retention).
