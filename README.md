# kquery

List Kubernetes objects across several kubeconfig contexts and namespaces and print, per object, the values of caller-chosen attribute paths. The default `tree` view nests the objects related to each root (Service, Ingress, Pod, Job, HPA, ScaledObject, PDB) and collects leftovers that belong to no root under `(unattached)`; `csv` and `table` print one flat row per root.

```bash
make build          # -> bin/kquery
make test vet       # unit tests, go vet
```

## Usage

```bash
kquery --ctx <context> [--ctx ...] --namespace <ns> [--namespace ...] [--kind <resource>] [--extra <path> ...] [--format tree|csv|table]
```

| Flag | Short | Default | Meaning |
|------|-------|---------|---------|
| `--ctx` | `-c` | required, repeatable | Kubeconfig context. Every context is combined with every namespace; contexts are queried concurrently and printed in the order given. |
| `--namespace` | `-n` | required, repeatable | Namespace to list in each context. |
| `--extra` | `-e` | none, repeatable | Attribute path printed as an extra column; the header is the path exactly as typed. In the tree view it is resolved on every node. |
| `--kind` | `-k` | `deployments` | Resource to list (the tree roots), in any form kubectl accepts: plural, singular, Kind, shortname or `resource.group` (`deploy`, `Deployment`, `svc`, `ingress`, `scaledobjects.keda.sh`). Resolved through API discovery per context. |
| `--kubeconfig` | | `$KUBECONFIG`, then `~/.kube/config` | Kubeconfig file. |
| `--format` | `-f` | `tree` | `tree` (roots with related objects nested, per ctx/namespace), `csv` (flat, RFC 4180, values quoted as needed) or `table` (flat, aligned columns). |
| `--group-label` | | `app.kubernetes.io/name` | Tree view: a Job belongs to a root when both carry this label with the same value. Empty disables Job grouping. |
| `--timeout` | | `30s` | Per-request timeout against each API server. |

Flat output columns are `ctx,namespace,name` followed by one column per `--extra`, in the order given. Rows are sorted by context (command-line order), then namespace (command-line order), then object name. The tree view prints one `ctx/namespace` block per combination in the same order. Resources are grouped under a Kind header line (which carries no values); names sit one indent (two spaces) under their Kind, and a node's related objects start one indent under its name with their own Kind headers. Roots are sorted by name; extra columns are aligned per block and their header is printed once per block.

Failures of a single context or namespace (unknown context, unreachable cluster, RBAC denied, namespace not found) go to stderr as `kquery: <ctx>[/<ns>]: <error>`; the other combinations are still printed. Exit code is `0` when every combination listed, `1` when any failed, `2` on a usage error (bad flag or path).

## Tree view

Roots are the objects of `--kind`. Related objects are listed once per ctx/namespace (one list call per kind) and matched in memory; a kind that is forbidden or absent is skipped with one stderr note (the KEDA `ScaledObject` CRD is skipped silently when the cluster does not have it). Children appear under a root in this order:

| Child | Attached to a root when | Nested beneath it |
|-------|-------------------------|-------------------|
| `Service` | `spec.selector` is a non-empty subset of the root's pod labels (`spec.template.metadata.labels`; a `Service` root uses its own selector) | `Ingress` objects whose `defaultBackend` or any rule path backend names the Service |
| `Pod` | its `ownerReferences` chain reaches the root (`Deployment → ReplicaSet → Pod`; ReplicaSets are followed but never shown). Roots without a pod template match Pods by selector instead | |
| `Job` | its `--group-label` value equals the root's (Jobs have no selector or owner link to a Deployment), or it is owned by the root (CronJob) | the Pods it owns |
| `HorizontalPodAutoscaler`, `ScaledObject` | `spec.scaleTargetRef.name` is the root's name and `kind`, when set, is the root kind | |
| `PodDisruptionBudget` | `spec.selector.matchLabels` is a non-empty subset of the root's pod labels | |

Everything of those kinds that matched no root is listed under a final `(unattached)` pseudo-root for that ctx/namespace, in the same kind order — this is where a Service + Ingress + PDB whose Deployment is gone, or an old Job, shows up. Ingresses stay nested under their Service there too; an Ingress whose Service no longer exists surfaces on its own. The line is omitted when nothing is unattached. An object may appear under several roots when it relates to each (a Job sharing the group label with two Deployments); the root kind itself is never listed as a child. Cluster-scoped roots get no children.

## Attribute paths

| Path form | Resolves to | Note |
|-----------|-------------|------|
| `labels.<key>` | `metadata.labels[<key>]` | The whole remainder after `labels.` is the key, verbatim — label keys contain dots and slashes (`labels.app.kubernetes.io/name`). Same for `annotations.<key>`, `metadata.labels.<key>`, `metadata.annotations.<key>`. |
| `a.b.c` | nested field | Split on `.`: `spec.progressDeadlineSeconds`, `status.readyReplicas`, `metadata.creationTimestamp`. |
| `a.b[0].c` | list index | `spec.template.spec.containers[0].image`, `status.conditions[0].status`. Quote these in zsh/bash — unquoted `[0]` is a glob pattern. |

Missing attribute → empty cell. Scalars print in their string form; maps and lists print as compact JSON.

## Examples

Tree of every Deployment in `shop` with its related objects, across two contexts (default format):

```bash
kquery --ctx prod-east --ctx prod-west -n shop \
  -e labels.app.kubernetes.io/name -e status.readyReplicas
```

```
prod-east/shop
name                             labels.app.kubernetes.io/name  status.readyReplicas
Deployment
  catalog                        catalog                        2
    Service
      catalog-svc                catalog
        Ingress
          catalog-ingress        catalog
    Pod
      catalog-7d9c6b8f4-q2xkz    catalog
      catalog-7d9c6b8f4-w8lmn    catalog
    PodDisruptionBudget
      catalog-pdb                catalog
  checkout                       checkout                       1
    Service
      checkout-svc               checkout
        Ingress
          checkout-ingress       checkout
    Pod
      checkout-67645d85f5-jjtzd  checkout
    HorizontalPodAutoscaler
      keda-hpa-checkout          checkout
    ScaledObject
      checkout                   checkout
    PodDisruptionBudget
      checkout-pdb               checkout
(unattached)
  Service
    cart-svc                     cart
      Ingress
        cart-ingress             cart
  Job
    checkout-e2e-5w6kw           checkout-e2e
  HorizontalPodAutoscaler
    cart-hpa                     cart
  PodDisruptionBudget
    cart-pdb                     cart

prod-west/shop
name                             labels.app.kubernetes.io/name  status.readyReplicas
Deployment
  checkout                       checkout                       2
    Service
      checkout-svc               checkout
        Ingress
          checkout-ingress       checkout
    Pod
      checkout-7945978df4-b6mpx  checkout
      checkout-7945978df4-j5rg4  checkout
    ...
```

(Excerpt; children trimmed with `...`. The `(unattached)` bucket is where the `cart` Service + Ingress + PDB whose Deployment is gone, an old end-to-end test Job and an HPA whose `scaleTargetRef` names a Deployment that no longer exists surface.)

The same roots flat, as CSV:

```bash
kquery --ctx prod-east --ctx prod-west -n shop -f csv \
  -e labels.app.kubernetes.io/name -e spec.progressDeadlineSeconds
```

```
ctx,namespace,name,labels.app.kubernetes.io/name,spec.progressDeadlineSeconds
prod-east,shop,catalog,catalog,600
prod-east,shop,checkout,checkout,600
prod-west,shop,checkout,checkout,600
```

Which rollout revision each Deployment is on, and what image it runs, as a table:

```bash
kquery -c prod-west -c prod-east -n shop -n payments -k deploy -f table \
  -e annotations.deployment.kubernetes.io/revision -e 'spec.template.spec.containers[0].image'
```

ClusterIPs of the Services (shortname) in one namespace, flat:

```bash
kquery -c prod-east -n shop -k svc -e spec.clusterIP -f csv
```

KEDA ScaledObject bounds (CRD, resolved through discovery), flat:

```bash
kquery -c prod-west -n shop -k scaledobjects -e spec.minReplicaCount -e spec.maxReplicaCount -f table
```

## Layout

| Path | Purpose |
|------|---------|
| `main.go` | Kong CLI, fan-out over contexts, child fetching, row/tree assembly, exit code (+ tests: flat/tree assembly, stderr format, flag parsing) |
| `internal/kube/` | Kubeconfig/context loading, kubectl-style resource resolution (discovery + shortcut expander), paginated dynamic list (+ tests: kubeconfig/context selection, no cluster needed) |
| `internal/attrpath/` | Path parser and resolver over unstructured objects (+ tests) |
| `internal/tree/` | Child kinds to fetch and the relationship matcher that builds the tree and the unattached bucket (+ tests) |
| `internal/printer/` | CSV, tabwriter table and tree output (+ tests) |
