# NomadNode runbook

`NomadNode` is a **representation** of a Nomad edge client — the operator
creates and deletes these CRs (reflecting Nomad's node registry); you read and
update them. It mirrors the Kubernetes `Node`/`cordon` model.

## Inspect the fleet
    kubectl get nomadnodes
    # NAME        CLUSTER   STATUS   ELIGIBLE     DRAINING   AGE

## Cordon a node (stop new placements, keep running allocs)
    kubectl patch nomadnode <name> --type=merge -p '{"spec":{"eligible":false}}'

## Drain a node (migrate allocs off, then it stays ineligible)
    kubectl patch nomadnode <name> --type=merge -p \
      '{"spec":{"drain":{"deadline":"1h","ignoreSystemJobs":true}}}'
    # Cancel a drain by removing the block:
    kubectl patch nomadnode <name> --type=json -p '[{"op":"remove","path":"/spec/drain"}]'

## Behavior notes
- **You cannot create or delete nodes here.** Deleting a NomadNode CR just
  re-mirrors on the next resync (~30s) if the node still exists; it never
  drains or deregisters the machine.
- **Spec wins over out-of-band changes.** If you run `nomad node drain -enable`
  from the CLI on a node whose CR has no `spec.drain`, the operator cancels it
  within one resync. Manage drains through the CR.
- **Down nodes stay visible** until Nomad garbage-collects them
  (`node_gc_threshold`, default 24h; set `NomadClusterSpec.nodeGCThreshold` to
  change it). Then the CR is pruned automatically.
- **Deleting the NomadCluster** cascades and removes all its NomadNode CRs.
