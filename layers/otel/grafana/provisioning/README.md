# Provisioning

Mounted at `/etc/grafana/provisioning` by `docker-compose.yaml`
(see [Provision dashboards and data sources](https://grafana.com/tutorials/provision-dashboards-and-data-sources/)).

- `datasources/datasources.yml`: one FoundationDB datasource on the compose
  stack's FDB (`docker:docker@fdb:4500`), database `/OTEL/R_GRAFANA_DEV`,
  tenants `T*`, default `T0`.

The stack starts with an empty cluster. Trace, search and logs queries need an
OTEL_V1 run database written by `otel.Exporter` (for example through
`layers/otel/cmd/otelbench` with `--run grafana_dev`); SQL works against any
relational database the `database` setting names.
