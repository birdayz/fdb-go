# Inspecting Records with fdbcli

## Connect to FDB
```bash
fdbcli
```

## Key Structure
Our records are stored with this structure:
- Subspace: `\x02conformance_test\x00` (tuple-encoded "conformance_test")
- Record subspace: `\x15\x01` (RecordKey constant = 1)
- Primary key: `\x16\x03\xe9` (tuple-encoded 1001) or `\x16\x07\xd2` (tuple-encoded 2002)
- Record type index: `\x14` (tuple-encoded 0)

## Commands to Find Records

### List all keys in conformance_test subspace:
```
fdb> getrange \x02conformance_test\x00 \x02conformance_test\x01
```

### Get specific record (order_id=1001):
```
fdb> get \x02conformance_test\x00\x15\x01\x16\x03\xe9\x14
```

### Get specific record (order_id=2002):
```
fdb> get \x02conformance_test\x00\x15\x01\x16\x07\xd2\x14
```

### List all in readable format:
```
fdb> getrange \x02conformance_test\x00 \x02conformance_test\x01 25
```
This shows up to 25 key-value pairs

## Understanding the Output
- Keys will show as hex strings
- Values are protobuf-encoded UnionDescriptor messages
- The value starts with `\x0a` (field 1 of UnionDescriptor)

## Useful Options
- Add `limit <n>` to limit results
- Use `begin` and `commit` for transactions
- Use `status` to check cluster health

## Example Session
```
$ fdbcli
Using cluster file `/etc/foundationdb/fdb.cluster'.
fdb> getrange \x02conformance_test\x00 \x02conformance_test\x01

Range limited to 25 keys
`\x02conformance_test\x00\x14' is `\n\t\n\x07_LATEST'
`\x02conformance_test\x00\x15\x01\x16\x03\xe9\x14' is `\n\x0f\x08\xe9\x07\x12\x08\n\x04Rose\x10\x01\x18\x19'
`\x02conformance_test\x00\x15\x01\x16\x07\xd2\x14' is `\n\x10\x08\xd2\x0f\x12\t\n\x05Tulip\x10\x02\x182'

fdb> get \x02conformance_test\x00\x15\x01\x16\x03\xe9\x14
`\x02conformance_test\x00\x15\x01\x16\x03\xe9\x14' is `\n\x0f\x08\xe9\x07\x12\x08\n\x04Rose\x10\x01\x18\x19'
```

## Tips
1. The hex strings can be hard to type - copy/paste is your friend
2. Use `help` in fdbcli to see all available commands
3. Exit with `exit` or Ctrl+D