# Running calbridge on a Google Compute Engine e2-micro VM

A small, always-on VM in Google Cloud's free tier, with no ports open to the internet. Expect about
**US$3.65 a month** (the VM's public IPv4 address). Everything else fits the free tier if you follow
the choices below.

This guide assumes you have already done the one-time setup in the README ("Running it", steps 1–3):
a working config, the Google OAuth client, the iCloud app-specific password, and a successful
`calbridge auth` and `calbridge once --dry-run` against the target calendar on your own machine.

Placeholders: `PROJECT_ID`, `BILLING_ACCOUNT_ID`, `HC_PING_URL`. Commands run on your Mac/PC unless
they are inside a `gcloud compute ssh … --command '…'`.

## What it costs

| Item | Choice | Monthly |
|---|---|---|
| VM | 1 × e2-micro in `us-west1`, `us-central1` or `us-east1` | $0 (always-free tier) |
| Disk | 20 GB **Standard** persistent disk | $0 (30 GB free). The console defaults to "Balanced", which is not free. |
| Public IPv4 | ephemeral, for outbound traffic only | ≈ US$3.65 (billed by the hour, used or not) |
| Outbound data | ≈ 0.2 GB including a 2-minute health ping | $0 (1 GB free) |
| Logs, budgets, IAP | | $0 |

calbridge only makes outbound connections, and every endpoint it uses (iCloud CalDAV, Google APIs,
healthchecks.io, Debian updates) has IPv6, so an IPv6-only VM could avoid the IPv4 charge. That
variant is untested; GitHub has no IPv6, so you would copy the binary in rather than clone.

## 1. Tools and project

```sh
brew install --cask google-cloud-sdk     # or Google's installer
gcloud auth login
```

Create a project in the Cloud Console and link a billing account (the free tier still needs one).
Project IDs are globally unique, so the ID usually has a numeric suffix: check it with
`gcloud projects list`.

```sh
P=PROJECT_ID
gcloud services enable compute.googleapis.com iap.googleapis.com billingbudgets.googleapis.com --project $P
```

## 2. Lock down the network

Enabling Compute Engine creates a `default` network whose rules allow **SSH, RDP and ping from the
whole internet**. Replace them with SSH from Google's Identity-Aware Proxy (IAP) only:

```sh
gcloud compute firewall-rules create allow-ssh-from-iap --project $P --network default \
  --direction INGRESS --action allow --rules tcp:22 --source-ranges 35.235.240.0/20 \
  --description "SSH only through Identity-Aware Proxy"
gcloud compute firewall-rules delete default-allow-ssh default-allow-rdp default-allow-icmp --project $P --quiet
```

The VM then accepts no connections from the internet; the status server stays on localhost.

## 3. Create the VM

```sh
gcloud compute instances create calbridge --project $P --zone us-west1-b \
  --machine-type e2-micro --image-family debian-12 --image-project debian-cloud \
  --boot-disk-size 20GB --boot-disk-type pd-standard \
  --no-service-account --no-scopes \
  --shielded-secure-boot --shielded-vtpm --shielded-integrity-monitoring \
  --metadata enable-oslogin=TRUE --labels app=calbridge
```

- `--no-service-account --no-scopes`: the VM has no credentials for anything else in the project.
- OS Login ties SSH to your Google identity; no SSH keys are left in project metadata.
- Google's Debian images have automatic security updates (`unattended-upgrades`) enabled already.

Check SSH over IAP (the first attempt on a fresh VM can fail while it boots; retry after a minute).
The first run creates `~/.ssh/google_compute_engine`.

```sh
SSH="gcloud compute ssh calbridge --project $P --zone us-west1-b --tunnel-through-iap"
$SSH --command 'cat /etc/debian_version; systemctl is-enabled unattended-upgrades'
```

## 4. Build and stage

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w -X main.version=$(git describe --always --dirty)" -o /tmp/stage/calbridge ./cmd/calbridge
cp deploy/calbridge.service /tmp/stage/
```

Make the server config from your working local config, changing only paths and logging:

| Key | Server value |
|---|---|
| `google.client_file` | `/etc/calbridge/google-client.json` |
| `google.token_file` | `/var/lib/calbridge/google-token.json` |
| `icloud.app_password_file` | `/etc/calbridge/icloud-app-password` |
| `store.path` | `/var/lib/calbridge/calbridge.db` |
| `store.backup_dir` | `/var/lib/calbridge/backups` |
| `log.level` / `log.format` | `info` / `json` |
| `server.listen` | `127.0.0.1:8089` |
| `monitoring.ping_url` | `HC_PING_URL` (step 7) |

Copy that config and the three secrets into the stage folder with mode 0600: the OAuth client JSON,
the Google token (reusing it from your machine means no browser step on the server), and the iCloud
app-specific password.

## 5. Install

```sh
$SSH --command 'rm -rf ~/calbridge-stage && mkdir -m 700 ~/calbridge-stage'
gcloud compute scp --project $P --zone us-west1-b --tunnel-through-iap \
  /tmp/stage/calbridge /tmp/stage/calbridge.service /tmp/stage/config.yaml \
  /tmp/stage/google-client.json /tmp/stage/google-token.json /tmp/stage/icloud-app-password \
  calbridge:calbridge-stage/
$SSH --command 'set -euo pipefail; cd ~/calbridge-stage
id calbridge >/dev/null 2>&1 || sudo useradd --system --home /var/lib/calbridge --no-create-home --shell /usr/sbin/nologin calbridge
sudo install -m 0755 -o root -g root calbridge /usr/local/bin/calbridge
sudo install -d -m 0750 -o root -g calbridge /etc/calbridge
sudo install -m 0640 -o root -g calbridge config.yaml /etc/calbridge/config.yaml
sudo install -m 0600 -o calbridge -g calbridge google-client.json icloud-app-password /etc/calbridge/
sudo install -d -m 0700 -o calbridge -g calbridge /var/lib/calbridge
sudo install -m 0600 -o calbridge -g calbridge google-token.json /var/lib/calbridge/
sudo install -m 0644 -o root -g root calbridge.service /etc/systemd/system/calbridge.service
sudo systemctl daemon-reload
sudo systemd-analyze verify /etc/systemd/system/calbridge.service
sudo systemd-analyze security calbridge.service | tail -1
shred -u google-client.json google-token.json icloud-app-password config.yaml; cd ~; rm -rf ~/calbridge-stage'
```

Then securely delete the local stage folder too (`rm -P` on macOS, `shred -u` on Linux).

Notes:

- The iCloud password file is owned by `calbridge` (not root) so that `status`, `reconcile` and
  dry runs work when run by hand as that user. The service itself also receives it as a systemd
  credential.
- `systemd-analyze security` should report an exposure level of about 1.4 ("OK").

## 6. Dry run, then start

Never start the service against a real calendar without reading a dry run from the server first:

```sh
$SSH --command 'cd /; sudo -u calbridge calbridge once --dry-run --config /etc/calbridge/config.yaml'
```

If the planned writes are what you expect (for a calendar already handled from another machine,
nothing), start it:

```sh
$SSH --command 'sudo systemctl enable --now calbridge; sleep 20; systemctl is-active calbridge; curl -s 127.0.0.1:8089/healthz'
```

Only one calbridge may sync a given calendar: stop any other copy (e.g. on your laptop) first.

## 7. Monitoring and budget

- **Dead-man's switch.** Create a free check at healthchecks.io (period 10 minutes, grace
  10 minutes), put its URL in `monitoring.ping_url`, and restart. calbridge pings after every
  successful cycle and pings `…/fail` on an authorization failure; you get an email when pings stop.
  Keep the URL private: anyone with it can send fake "all good" pings.
- **Budget alert.** Budgets only alert; they never stop spending. Use the billing account's
  currency, and set the amount above the expected ~US$3.65 so it doesn't fire every month (for an
  AUD account, A$10). The Budgets API needs a quota project, hence `--billing-project`:

  ```sh
  gcloud billing accounts describe BILLING_ACCOUNT_ID --format="value(currencyCode)"
  gcloud billing budgets create --billing-account=BILLING_ACCOUNT_ID --billing-project=$P \
    --display-name="calbridge monthly" --budget-amount=10AUD --filter-projects=projects/$P \
    --threshold-rule=percent=0.9 --threshold-rule=percent=1.0 \
    --threshold-rule=percent=1.0,basis=forecasted-spend
  ```

## Operating it

```sh
$SSH --command 'sudo -u calbridge calbridge status --config /etc/calbridge/config.yaml'   # syncs, mirrors, retries
$SSH --command 'sudo journalctl -u calbridge -n 50 --no-pager'                            # recent logs
$SSH --command 'curl -s 127.0.0.1:8089/status'                                            # counts of the last cycle
```

At `log.level: info`, individual writes are not logged (only start-up, backups, warnings and
errors); `/status` and `calbridge status` show what each cycle did.

**Upgrade calbridge:** build as in step 4, copy just the binary, then:

```sh
$SSH --command 'sudo install -m 0755 -o root -g root ~/calbridge /usr/local/bin/calbridge && rm ~/calbridge && sudo systemctl restart calbridge && calbridge version'
```

**Rotate a secret:** copy the new file over IAP, `sudo install -m 0600 -o calbridge -g calbridge`
it to the same path, `sudo systemctl restart calbridge`. A new Google token comes from running
`calbridge auth` locally and copying `google-token.json`, or on the server through an SSH tunnel as
described in the README.

**Restore a backup:** see the README ("Backups and restore"); the backups are in
`/var/lib/calbridge/backups` on the VM.

**Remove everything:** `gcloud compute instances delete calbridge --project $P --zone us-west1-b`,
then the firewall rule and the budget, or delete the whole project.
