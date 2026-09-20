# Running Several Replicas (Clustering)

## What it is for

By default each Haribon replica decides on its own which servers are healthy, so
two replicas can disagree about the same server at the same time. One will keep
sending traffic to a server the other has given up on.

Turn on `cluster` and the replicas tell each other what they find, so a server
that any replica cannot reach is skipped by all of them.

```
client -> load balancer (k8s Service, cloud LB, or DNS)
              |
              +-- haribon-0 ---+
              +-- haribon-1 ---+--> servers 4441, 4442, 4443
              +-- haribon-2 ---+
                   |  |  |
                   +--+--+--- health findings over UDP 7946
```

Each replica proxies requests independently. Nothing is coordinated apart from
health findings.

## Configuration

```yaml
cluster:
  enabled: true
  node_id: "${HOSTNAME}"        # must be different on every replica
  peers:                        # every replica, this one included
    - "haribon-0:7946"
    - "haribon-1:7946"
    - "haribon-2:7946"
  gossip_interval_sec: 5        # how often to exchange findings
  gossip_addr: "0.0.0.0:7946"   # the address this replica listens on
```

`node_id` is expanded from the environment, so `${HOSTNAME}` gives each container
or pod its own name. If a `${...}` reference cannot be resolved, `start` refuses
to run — two replicas sharing one node name would ignore each other's findings
completely, which looks exactly like clustering that works until a server fails.

Listing a replica in its own `peers` is allowed and is the simplest thing to do
when every replica reads the same ConfigMap: a message a replica sends to itself
is discarded on arrival and is not counted as a peer.

If the gossip port cannot be opened, `start` exits with an error rather than
running half-connected.

## What gets shared

Only health findings: which server each replica currently believes is up or
down. Nothing else travels between replicas.

| | Shared between replicas |
|---|---|
| Server health (up/down) | Yes |
| Circuit breaker state, retry counters | No — each replica keeps its own |
| Config file contents | No — see "Config drift" below |
| Request traffic | No — put the replicas behind something that balances |

Because findings are the only thing exchanged, a replica's own measurements stay
authoritative for its own routing decisions. Clustering adds a second opinion; it
does not replace local health checking.

### How a finding travels

When replica A marks a server unhealthy:

1. A records the finding with a sequence number higher than anything it has sent
   before, and includes it in its next round of messages.
2. B and C receive it, and adopt it because its sequence number is newer than
   whatever they held for that server.
3. All three replicas now skip that server.

A replica sends only the findings it made itself. It never passes a peer's
finding on, because a forwarded finding would be re-sent by each replica in turn
and would outlive the replica that actually observed the problem.

This means **every replica must be able to reach every other replica directly**:
the `peers` list needs to be complete on every replica, not a chain or a ring.

## When a replica goes away

A peer's finding is only trusted while that peer keeps reporting it. If nothing
is heard from a replica for about three gossip rounds (never less than 15
seconds), its findings are dropped and each remaining replica goes back to its
own measurements.

This matters most during a rolling update. A replica shutting down may have been
the one that marked a server unhealthy. If its finding were kept forever, that
server would stay out of rotation on every replica with nothing left to clear
it — a server that is up and serving would get no traffic, and no amount of
restarting the healthy replicas would fix it. Dropping the finding after the
timeout means the remaining replicas re-decide for themselves.

The trade-off: a replica that is genuinely down keeps influencing routing for up
to three gossip rounds. Lower `gossip_interval_sec` if that window matters more
to you than the extra traffic.

## Config drift

Each replica sends a fingerprint of its own config file. If a peer's fingerprint
does not match, the receiving replica:

- counts it in `haribon_config_hash_mismatch_total`, and
- logs a line naming the peer and the two fingerprints.

It logs the first mismatch and then every 30th, so a long-lived drift does not
fill the log.

This is how you catch a rollout that only reached some of the replicas. Nothing
is repaired automatically: replicas keep running their own config files. `/readyz`
is not affected, so a replica with a drifted config still reports itself ready —
watch the metric or the log if you care about drift.

Reloading works per replica. A `SIGHUP` or `--watch_config` on one replica
reloads that replica only; the others pick up their own copy of the file when
they are reloaded too.

## Peer addresses

Peers are ordinary `host:port` strings, resolved when each round is sent.

A name that resolves to several addresses is **not** a way to list peers: only
one of the addresses is used per send, chosen by the resolver, so results would
be unpredictable. In Kubernetes, list the replicas explicitly, or use the pod
names from a StatefulSet, rather than pointing at a Service name that fronts
them all.

## Rolling updates

```bash
kubectl rollout restart deployment/haribon
```

Each pod, in turn:

1. Receives `SIGTERM`.
2. Stops accepting new connections and finishes the requests already in flight
   (up to `shutdown_timeout_sec`).
3. Stops talking to its peers. Its findings are dropped by the others after the
   expiry window described above, and its traffic is carried by the replicas
   still running.

The remaining replicas keep serving throughout. Nothing in Haribon coordinates
which pod goes when — that is the deployment's job.

## Metrics

| Metric | Meaning |
|---|---|
| `haribon_cluster_peers` | Replicas heard from recently |
| `haribon_cluster_term` | Highest update counter this replica has recorded, its own or a peer's |
| `haribon_config_hash_mismatch_total` | Peer config fingerprints that differ from this replica's |

`haribon_cluster_peers` counts replicas that have actually sent a message
recently, not the addresses in `peers`. A configured peer that has never been
heard from is not evidence of anything, and a peer that went away has to leave
the count.

```bash
curl -s localhost:4444/metrics | grep -E 'cluster_peers|cluster_term|config_hash'
```

## Kubernetes

```bash
kubectl apply -f samples/k8s/manifests.yml
kubectl scale deploy/haribon --replicas=5
```

The manifest includes a Deployment, a Service on port 4444 for client traffic, a
headless Service the replicas use to address each other, an HPA, and a
PodDisruptionBudget. The gossip port (7946/UDP) is separate from the traffic
port and is not exposed outside the cluster.

## Checklist

- [ ] `cluster.enabled: true`, with `node_id` unique per replica
- [ ] `peers` lists every replica, identically, on every replica
- [ ] Gossip port 7946/UDP reachable between replicas
- [ ] Client traffic goes to the Service on 4444, not to the gossip port
- [ ] PodDisruptionBudget keeps enough replicas available during updates
- [ ] Alert on `haribon_config_hash_mismatch_total` so a partial rollout is seen
- [ ] After killing a replica, confirm `haribon_cluster_peers` drops and recovers

## What clustering does not do

- **No traffic balancing between replicas.** Haribon does not distribute requests
  across the replicas; a load balancer, a Service, or DNS in front of them does.
- **No configuration replication.** Replicas read their own config files.
- **No leader election and no external consensus store.** Findings are hints, not
  a source of truth, so there is nothing to elect a leader for and no etcd or
  Consul to run.
- **No disk state.** A replica needs no writable filesystem and no root to take
  part.
- **No sticky sessions across replicas with different server lists.** `ip_hash`
  picks a server by hashing the visitor's address against the server list, so
  identical replicas pin the same visitor to the same server. Replicas whose
  lists differ — different config files, or a discovery source that returns
  different results per replica — may pin that visitor to different servers.
