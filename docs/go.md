# Go integration

Use `github.com/shenli/ramet/engine` to embed Ramet in your process, or
`github.com/shenli/ramet/client` to call an HTTP worker. Provider interfaces and
implementations live in `github.com/shenli/ramet/storage`. The HTTP server is an
internal implementation package; the supported entry point for a worker is
`ramet serve`.

## Embedded engine

The engine owns one private worker data directory. Given a context `ctx` and a
quiesced source directory outside the worker data directory:

```go
import "github.com/shenli/ramet/engine"

// Inside a function returning error:
store, err := engine.Open("/tmp/ramet-data", engine.Options{})
if err != nil {
    return err
}
defer store.Close()

base, err := store.Snapshot(ctx, engine.SnapshotOptions{Source: "/path/to/source"})
if err != nil {
    return err
}
pool, err := store.CreatePool(ctx, engine.PoolOptions{Base: base.ID, Concurrency: 16})
if err != nil {
    return err
}
defer pool.Close()

execution, err := pool.Acquire(ctx)
if err != nil {
    return err
}
// Mount execution.Path in your runtime. Stop its writers and unmount before release.
return pool.Release(ctx, execution)
```

`Reset` returns a new execution path and generation; replace the old handle with
its return value. Renew leases during long attempts. Pools and executions are
ephemeral; snapshot, checkpoint, or promote explicitly to preserve state.
See the [lifecycle design](design.md) and [security model](../SECURITY.md).

## HTTP client

The `client` package uses the same engine request/response types and talks to
the worker's HTTP API. Returned paths belong to the worker's filesystem; the
client does not transfer workspace files to another host. See
[API and CLI](operations.md#api-and-cli) for routes and authentication, and
[client.go](../client/client.go) for the Go bindings.

## Storage providers

The public `storage.Metadata` and `storage.Content` interfaces let embedded
applications supply durable providers. The repository includes PostgreSQL and
S3-compatible implementations. Configure both together; remote mode requires
both metadata and content durability. See [storage modes](architecture.md#storage-modes)
and the [provider contracts](../storage/storage.go).


## Recover an interrupted mutation

Obtain `id, err := c.NewOperation(ctx)` and persist the ID before the request.
Use `operationCtx := client.WithOperationID(ctx, id)` for that mutation.
If the response is lost, `*client.OutcomeUnknownError` includes the ID; inspect
`c.Operation(ctx, id)` or retry the identical method and arguments using the same
context/ID. Never create a new ID just to retry an uncertain mutation.

Receipt lifetime, worker restart, capacity, and stale-lease boundaries are
specified in the [agent integration guide](agents.md#recover-an-uncertain-mutation).
The Go client does not automatically retry or refresh a handle returned by a
historical receipt; renew/verify it before resuming work. `*client.Error` includes
`RequestID`, `RetryAfter`, and `OperationID` alongside status/code/message.
