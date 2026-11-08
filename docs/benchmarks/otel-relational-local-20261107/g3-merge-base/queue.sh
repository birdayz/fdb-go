#!/bin/bash
# Prints the worst storage/log queue bytes of otelg3mb.
docker exec otelg3mb fdbcli --exec "status json" --timeout 20 2>/dev/null | python3 -c "import json,sys; q=json.load(sys.stdin)['cluster']['qos']; print(max(q['worst_queue_bytes_storage_server'], q['worst_queue_bytes_log_server']))"
