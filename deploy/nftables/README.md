# Egress policy for the instance account

`dabberz-egress.nft` restricts what the account customers' apps and agents run
as (`driver.local.run_as`, `dabberz` on dbz-alpha) may connect to. The daemon
runs as root and is deliberately untouched.

## What it is for

On the local driver there is no network separation between a customer's app
and anything else on the machine. An app that can make an outbound request can
reach the control plane on `:80`, the Postgres cluster, and — the one that
matters — the preview range `41000-42999`, where every other installation is
listening on a predictable, contiguous port.

A connection made there directly never passes through the router, which is the
only place a visitor's identity headers are stripped and the real one asserted
against route ownership. Everything that work bought is bypassable from inside
the box. Finding a neighbour is a for-loop.

## What it does not do

**It is not tenant isolation.** Every instance runs as the same uid, so a rule
matching on uid cannot tell one customer's app from another's. Blocking the
whole preview range closes the cross-tenant path with a blunt instrument; it
does not give each tenant a boundary. That needs a uid per installation, or the
Firecracker driver (task #18).

What it does buy is the abuse surface that gets a host's address nullrouted:
no mail relay, no private-network or link-local access, no arbitrary outbound
ports, no reaching the control plane, and a log line for anything that tries.

## Two things to know before loading it

**A multi-service app will break.** The rules refuse loopback except Postgres
and DNS, so a project whose services talk to each other over `localhost` loses
that. Forms is a single service, so nothing breaks today — but reconsider this
before shipping an app with a web tier and a separate API.

**A blocked connection may hang rather than fail.** `reject` in the output
hook is supposed to return an error to the local socket immediately. On some
kernels it behaves like `drop`, and the caller waits for its own timeout
instead. That turns a blocked request into a stall — an agent run that waits
out `agent.timeout` rather than erroring in a second. `verify-egress.sh` says
which behaviour this machine has; if it reports timeouts, weigh that against
the protection before leaving the rules in place.

## Installing

The rules live in their own table, so they coexist with whatever else is
loaded — notably the rules restricting port 80 to the CDN's ranges. **Do not
put this in `/etc/nftables.conf`**, and do not replace that file.

```sh
sudo install -d /etc/nftables.d
sudo install -m 0644 deploy/nftables/dabberz-egress.nft /etc/nftables.d/

# Check it parses before it is loaded.
sudo nft -c -f /etc/nftables.d/dabberz-egress.nft

# Load it. Takes effect immediately; existing connections are unaffected.
sudo nft -f /etc/nftables.d/dabberz-egress.nft

# Confirm it does what it claims, on this machine.
sudo bash deploy/nftables/verify-egress.sh dabberz
```

Nothing above can lock you out: every rule is in the `output` hook and matches
only the instance account. Inbound SSH, and root's own traffic, are untouched.

### Keeping it across a reboot

Append the include to `/etc/nftables.conf` rather than editing the rules into
it, so the two stay separate:

```sh
grep -q 'nftables.d' /etc/nftables.conf || \
  echo 'include "/etc/nftables.d/*.nft"' | sudo tee -a /etc/nftables.conf
sudo systemctl enable --now nftables
```

## Rolling back

```sh
sudo nft delete table inet dabberz_egress     # immediate, nothing else affected
sudo rm /etc/nftables.d/dabberz-egress.nft    # and so it does not come back
```

## Watching it

Refusals are logged with a prefix saying which rule caught them:

```sh
journalctl -k -f | grep dabberz-egress
nft list table inet dabberz_egress            # counters per rule
```

`dabberz-egress loopback` on a running installation means an app tried to
reach the control plane or a neighbour. Worth looking into rather than
assuming it is noise.
