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

Garage is a good fit for a self-hosted, single-node or small-cluster store. These steps set up one node next to notify, and were verified with Garage 2.4.1.

1. Write a config. A single node keeps one copy of each object and listens on loopback only; generate the secrets on the host and keep the file readable by its owner only (`chmod 600`):

   ```toml
   metadata_dir = "/var/lib/garage/meta"
   data_dir = "/var/lib/garage/data"
   db_engine = "lmdb"
   metadata_auto_snapshot_interval = "6h"
   replication_factor = 1

   rpc_bind_addr = "127.0.0.1:3901"
   rpc_public_addr = "127.0.0.1:3901"
   rpc_secret = "<openssl rand -hex 32>"

   [s3_api]
   s3_region = "garage"
   api_bind_addr = "127.0.0.1:3900"
   root_domain = ".s3.garage.localhost"

   [admin]
   api_bind_addr = "127.0.0.1:3903"
   admin_token = "<openssl rand -base64 32>"
   metrics_token = "<openssl rand -base64 32>"
   ```

2. Run `garage -c garage.toml server` under your service manager, then give the node a role. The capacity weights how data is placed; it is not a quota:

   ```bash
   garage -c garage.toml status                       # note the node id
   garage -c garage.toml layout assign -z dc1 -c 20G <node-id>
   garage -c garage.toml layout apply --version 1
   ```

3. Create a bucket and a key, and allow the key to read and write it:

   ```bash
   garage -c garage.toml bucket create notify-mail
   garage -c garage.toml key create notify            # prints the key id and secret once
   garage -c garage.toml bucket allow --read --write --owner notify-mail --key notify
   ```

4. Point notify at port 3900 with that key, region `garage`, and `NOTIFY_BLOB_S3_TLS` off, then restart notify.

`GET http://127.0.0.1:3903/health` answers `200` while the node can serve reads and writes; watch it from your monitoring. To add nodes later, bind `rpc_bind_addr` and `rpc_public_addr` to an address the other nodes can reach, raise `replication_factor`, and assign each node a zone in the layout.

### Moving from fs to s3

Both adapters use the same `ab/cd/<sha256>` layout (with `NOTIFY_BLOB_S3_PREFIX` unset), so moving is a copy of each file to the same key:

1. Copy every file under the `fs` directory into the bucket under its relative path, with any S3 client (`aws s3 sync`, `rclone`, or `curl --aws-sigv4 "aws:amz:garage:s3"`).
2. Switch `NOTIFY_BLOB_DRIVER` to `s3` and restart notify.
3. Copy again, to catch mail accepted onto disk before the restart. Re-copying an object is harmless.
4. Check that the bucket's object count is at least `SELECT count(DISTINCT sha256) FROM inbound_messages`, and fetch a few messages through `/v1/inbound/messages/{id}/raw`, before removing the `fs` directory.

Garage listens on plain HTTP unless you terminate TLS in front of it, so leave `NOTIFY_BLOB_S3_TLS` off for a loopback or private-network endpoint, and turn it on if it is behind HTTPS.

## Failure behaviour

If the blob store is configured and a write fails while accepting inbound mail, the message is **not** accepted: the SMTP receiver answers `451` and the JSON webhook answers `500`, so the sender retries. Nothing is stored twice on the retry. If an archived object has gone missing from the store, `GET /v1/inbound/messages/{id}/raw` answers `404` with "the message's bytes are no longer stored".

## Backups

The database and the blob store are separate. A database backup contains the records (id, hash, sender, subject) but not the bytes. Back up the `fs` directory or the bucket on its own schedule. See [Backups and retention](../backups-and-retention).
