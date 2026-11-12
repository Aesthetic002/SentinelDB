---
tags: [sentinel-db, security, threat-model]
aliases: [SentinelDB Security]
created: 2026-10-13
updated: 2026-11-04
---

# Security — What's Protected and What Isn't

Back to [[1 - Overview|Overview]]. Being precise about what this does and doesn't defend against is more convincing to anyone evaluating it than just claiming "it's secure" — so this note is written as an honest threat model, not a marketing list.

## The most important thing to say first: this protects against crashes, not lies

Everything in [[3 - How Leader Election Works|the consensus protocol]] — including the safety proof in [[5 - How We Prove It Cant Break|How We Prove It Can't Break]] — assumes that every node still running is telling the truth. It protects beautifully against a node going silent, slow, or unreachable. It does **not** protect against a node that keeps running but deliberately sends false information (fake votes, corrupted log entries). That second, much harder problem has a name — Byzantine fault tolerance — and solving it requires a genuinely different and more expensive family of algorithms. This project doesn't attempt it, and says so plainly: "the system tolerates up to roughly half the nodes crashing or becoming unreachable, and assumes every node that's still participating is honest."

## Keeping the network traffic itself safe

- **Between nodes:** connections use mutual TLS (both sides present a certificate from the cluster's own certificate authority), and each certificate names one specific node. A machine without the right certificate can't impersonate a node or inject fake protocol messages into the cluster, and a node can't send messages claiming to be from a different node, because the sender's identity comes from its certificate. Each of these was tested by actually attempting it: no certificate, a client's certificate on the node port, a certificate from another authority, and one node impersonating another.
- **Between a client and a node:** TLS encrypts the connection and proves the client reached a real node, and a token decides whether a given client is allowed to connect at all, and whether it can read, write or administer. Nodes only store a hash of each token. Client certificates are optional by default, and starting a node with `-require-client-cert` makes them mandatory as well, giving full mutual TLS.
- **Changing who's in the cluster:** which nodes belong to the group is fixed when the cluster starts; adding or removing a node later requires an explicit, authenticated administrative action — never something a normal client request could trigger. This closes off an attack where someone could otherwise try to sneak in a rogue node, or remove enough honest nodes to break the majority the whole system depends on.

## Why SQL injection isn't a realistic concern here

Because every query is parsed through the formal grammar described in [[5 - How We Prove It Cant Break|How We Prove It Can't Break]] into a structured parse tree *before* anything is executed, and any value a user supplies is treated as data bound in afterward rather than spliced directly into the query text beforehand, the classic SQL-injection attack doesn't apply here by construction — as long as that discipline (never build a query by gluing a user's value into raw text) is kept everywhere in the implementation. The grammar also allows only one statement per request, so `x; DROP TABLE t` is a syntax error. Real injection payloads are tested against the full executor: each one is stored and compared as an ordinary string.

## Keeping stored data honest

Every piece of data written to disk — log entries, snapshots — carries a checksum, so that if a disk error or a latent bug silently corrupts something, it's detected on the next read instead of being quietly treated as valid and replayed into the live system. A crash halfway through a write and real corruption get treated differently: damage at the very end of the log is what a crash leaves behind, so it's discarded (it was never acknowledged), while damage with valid data after it can't be explained by a crash, so the node refuses to start. Tests cut the log at every byte, and flip bits at every byte. A flip inside the very last record can't be told apart from an interrupted write, so that record is dropped, which is safe for the same reason.

## The one genuinely interesting, non-obvious risk this project introduces

Because the proactive-handover feature (see [[6 - How Failure Prediction Works|How Failure Prediction Works]]) reacts to the *pattern* of a node's heartbeats, a node that's been compromised — or is simply misbehaving — could deliberately send noisy, degraded-looking heartbeats without actually failing, tricking the classifier into triggering unnecessary leadership handovers. That's a subtle kind of denial-of-service that specifically targets the exact feature that makes this project novel in the first place. We're not pretending to defend against this — it's documented here as a real, interesting limitation and a natural next question for future work, because recognizing that adding intelligence to a system also hands an attacker a new kind of lever to pull is itself a meaningful piece of understanding, even without a solution attached to it yet.

A related point the safety check does establish: because the model lets a leader hand over at any moment for no reason at all, this attack can cause needless handovers and a loss of availability, but it can't cause split leadership or lost data.

## Smaller limits worth knowing

- **A removed node's certificate stays valid.** There's no certificate revocation. A removed node can't vote its way back in, but keeping its certificate from being used is left to whoever runs the cluster.
- **Fault injection is a real lever.** The experiments need a way to slow a node down deliberately, so it exists, but nodes refuse it unless started with `-enable-chaos`, and even then it needs an admin token. Never start a production node with that flag.
- **The demo dashboard can kill nodes.** So it only listens on the local machine, and refuses requests coming from other websites.

## Summary table

| In scope — actually implemented and tested | Out of scope — named as a real limitation |
|---|---|
| Mutual TLS between nodes | Byzantine fault tolerance (nodes that lie rather than crash) |
| TLS plus role-based tokens for clients, with mutual TLS available via `-require-client-cert` | Adversarial robustness of the ML predictor against deliberately crafted heartbeat patterns |
| Membership changed only via an authenticated admin action | A full cryptographic audit of the TLS configuration |
| Injection-safe query execution via the formal grammar | Certificate revocation |
| Checksummed log and snapshot storage | |
