# Strict only. No warning arm, even if a caller supplies allow_batch_warning.
def nonnegative: type == "number" and . >= 0 and . < 1e300;
def bytes: nonnegative and floor == .;
def hasrole($name): any(.roles[]; .role == $name);
def role_placement:
  if .address == "127.0.0.1:14883" or .address == "127.0.0.1:14886" then
    .class_type == "storage" and hasrole("storage")
    and (hasrole("log") or hasrole("commit_proxy") or hasrole("grv_proxy") or hasrole("resolver") | not)
  elif .address == "127.0.0.1:14884" then
    .class_type == "transaction" and hasrole("log")
    and (hasrole("storage") or hasrole("commit_proxy") or hasrole("grv_proxy") or hasrole("resolver") | not)
  elif .address == "127.0.0.1:14885" then
    .class_type == "stateless" and hasrole("commit_proxy") and hasrole("grv_proxy")
    and hasrole("resolver") and hasrole("master") and hasrole("cluster_controller")
    and (hasrole("storage") or hasrole("log") | not)
  else false end;
def optional_configuration_messages_empty:
  if (.client | has("configuration")) then
    if (.client.configuration | has("messages")) then
      .client.configuration.messages | type == "array" and length == 0
    else true end
  else true end;
def required_probes_succeeded:
  all([.cluster.latency_probe.transaction_start_seconds,
       .cluster.latency_probe.immediate_priority_transaction_start_seconds,
       .cluster.latency_probe.read_seconds,.cluster.latency_probe.commit_seconds][];
      nonnegative and . < 5);
def strict_aggregate:
  .client.database_status.healthy == true and (.cluster.messages | length == 0);
# jq normally streams multiple root values; one admin response must be one document.
if ([limit(1; inputs)] | length) != 0 then ("Expected exactly one status document\n" | halt_error(1)) else . end |
if (.client.database_status.available == true
    and (.client.database_status.healthy | type == "boolean")
    and .client.coordinators.quorum_reachable == true
    and .client.cluster_file.up_to_date == true
    and (.client.messages | type == "array" and length == 0)
    and optional_configuration_messages_empty
    and (.cluster.messages | type == "array")
    and .cluster.database_available == true
    and .cluster.data.state.healthy == true
    and .cluster.recovery_state.name == "fully_recovered"
    and .cluster.degraded_processes == 0
    and .cluster.configuration.storage_engine == "ssd-redwood-1"
    and .cluster.configuration.log_engine == "ssd-2"
    and .cluster.configuration.redundancy_mode == "single"
    and .cluster.configuration.logs == 1
    and .cluster.configuration.commit_proxies == 1
    and .cluster.configuration.grv_proxies == 1
    and .cluster.configuration.resolvers == 1
    and (.cluster.qos.worst_queue_bytes_storage_server | bytes)
    and (.cluster.qos.worst_durability_lag_storage_server.seconds | nonnegative)
    and (.cluster.qos.performance_limited_by | type == "object")
    and (.cluster.qos.performance_limited_by.name | type == "string" and length > 0)
    and required_probes_succeeded
    and (.cluster.processes | type == "object" and length == 4)
    and ([.cluster.processes[].address] | sort) == ["127.0.0.1:14883","127.0.0.1:14884","127.0.0.1:14885","127.0.0.1:14886"]
    and all(.cluster.processes[];
      (.memory.rss_bytes | bytes)
      and (.memory.used_bytes | bytes)
      and .memory.limit_bytes == 4294967296
      and .locality.machineid == "otel283-vxnar-ingest20k"
      and .locality.zoneid == "otel283-vxnar-ingest20k"
      and (.messages | type == "array" and length == 0)
      and (.roles | type == "array" and length > 0)
      and all(.roles[]; .role | type == "string" and length > 0)
      and role_placement)
    and strict_aggregate)
then {aggregate_healthy_original:.client.database_status.healthy,
      health_exception_used:false,
      warning_names:[.cluster.messages[].name],
      queue_bytes:.cluster.qos.worst_queue_bytes_storage_server,
      durability_lag_seconds:.cluster.qos.worst_durability_lag_storage_server.seconds,
      limiting_reason:.cluster.qos.performance_limited_by,
      batch_limiting_reason:.cluster.qos.batch_performance_limited_by,
      latency_probe:.cluster.latency_probe,
      storage_engine:.cluster.configuration.storage_engine,
      recovery_state:.cluster.recovery_state.name,
      data_state:.cluster.data.state,
      processes:[.cluster.processes | to_entries[] |
        {id:.key,address:.value.address,class_type:.value.class_type,
         memory:{rss_bytes:.value.memory.rss_bytes,used_bytes:.value.memory.used_bytes,limit_bytes:.value.memory.limit_bytes},
         roles:[.value.roles[].role]}]}
else error("Strict status unhealthy/malformed; no warning exceptions") end
