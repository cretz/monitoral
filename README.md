# Monitoral

(under development)

## Development

### Regen protos

```
export TEMPORAL_PROTO_ROOT=$(go list -m -f '{{.Dir}}' github.com/cretz/temporal-sdk-go-advanced/temporalproto)
export GOOGLE_PROTO_ROOT=/path/to/protoc/include
protoc --go_out=paths=source_relative:. --go_temporal_out=paths=source_relative:. -I . -I $TEMPORAL_PROTO_ROOT -I $GOOGLE_PROTO_ROOT ./monitoralpb/monitoral.proto
```