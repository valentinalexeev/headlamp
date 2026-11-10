# Backup, restore and a recovery drill for the control host

These files are a sample. They add continuous backup to S3-compatible storage to `examples/compose/control`. They also give a restore for a new host and a recovery drill that causes no downtime. Copy them to your private repository and change them for your site.

The daily volume snapshot (see "Backups" in `../README.md`) can lose up to one day of changes. These files lose about 10 seconds of the databases.

## What each part keeps

| Data | How | Recovery point |
|---|---|---|
| headscale database (`headscale/db.sqlite`): users, nodes, keys | Litestream, continuous | about 10 seconds |
| node-control policy console history (`node-control/policy.db`), when `POLICY_DB` is set | Litestream, continuous | about 10 seconds |
| node-control enrolment store (`node-control/enroll.db`), when `ENROLL_DB` is set | Litestream, continuous | about 10 seconds |
| headscale noise and DERP private keys | `backup.sh`, each night | one day (the keys do not change) |
| node-control enrolment export, when `ENROLL_REDIS=1` | `backup.sh`, each night | one day |
| Redis snapshot, when the compose has a `redis` service | `backup.sh`, each night | one day |

**WARNING:** Keep the headscale keys and the node-control secrets in a safe location. Without the noise key, every client must log in again. Without `ENROLL_SECRET_KEY` and `NODE_CONTROL_STATE_KEY`, node-control cannot read its restored data.

## Files

| File | Purpose |
|---|---|
| `litestream.yml` | The databases that Litestream replicates. Remove the entries that your site does not use. |
| `compose.backup.yaml` | A compose overlay that adds the `litestream` service. |
| `backup.sh` | The nightly copy of the data that Litestream does not replicate. |
| `restore.sh` | Puts the data back on a new host before the first start. |
| `drill.sh` | Restores into a temporary directory on the live host and compares the result with the live data. |

## Set up

1. Make a bucket with versioning on. Versioning keeps older copies of the nightly files.
2. Give the host write access to the bucket. On a cloud VM, use the instance role, not a key.
3. Add these values to `.env`:

   ```sh
   BACKUP_BUCKET=<bucket>
   BACKUP_REGION=<region>
   LITESTREAM_IMAGE=litestream/litestream:<version>@sha256:<digest>
   AWS_CLI_IMAGE=amazon/aws-cli:<version>@sha256:<digest>
   # Only with a redis service:
   REDIS_IMAGE=<the same image as the redis service>
   ```

4. Start the stack with the overlay:

   ```sh
   docker compose -f compose.yaml -f backup/compose.backup.yaml up -d
   ```

5. Run `sh backup/backup.sh` one time, and make sure that it prints the sizes.
6. Run `backup.sh` each night from a systemd timer. Run it before the daily volume snapshot. A sample unit:

   ```ini
   # /etc/systemd/system/headlamp-backup.service
   [Service]
   Type=oneshot
   WorkingDirectory=<compose directory>
   ExecStart=/bin/sh backup/backup.sh

   # /etc/systemd/system/headlamp-backup.timer
   [Timer]
   OnCalendar=*-*-* 02:30:00 UTC
   Persistent=true
   [Install]
   WantedBy=timers.target
   ```

7. Run `systemctl enable --now headlamp-backup.timer`.

## Restore on a new host

**CAUTION:** Do not start the stack before the restore. headscale makes a new, empty database on its first start.

1. Make the new host, and copy the compose directory, `.env` and the secret files to it.
2. Run `sh backup/restore.sh` in the compose directory.
3. Make sure that the output does not show a WARNING line.
4. Start the stack with the overlay.
5. Make sure that the nodes come online in `headscale nodes list`.

`restore.sh` does nothing when `headscale/db.sqlite` exists. It never writes over a live database.

## Do a drill

Do a drill after each change to the backup, and one time each quarter.

1. Run `sh backup/drill.sh` in the compose directory on the live host.
2. Make sure that the output shows `databases and keys MATCH`.
3. Write down the restore time. This time is your recovery time without the time for a new host.

The drill writes only to a temporary directory. It prints counts and hashes, not values. A count can be different by one or two rows when the live service writes during the drill. Run the drill again before you look for a fault.
