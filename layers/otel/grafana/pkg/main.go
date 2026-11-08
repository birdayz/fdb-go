// Command gpx_foundationdb is the FoundationDB Grafana datasource backend.
package main

import (
	"os"

	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"fdb.dev/layers/otel/grafana/pkg/plugin"
	"fdb.dev/pkg/fdbgo/fdb"
)

func main() {
	if err := fdb.APIVersion(plugin.APIVersion); err != nil {
		log.DefaultLogger.Error("select FDB API version", "error", err)
		os.Exit(1)
	}
	if err := datasource.Manage(plugin.PluginID, plugin.NewDatasource, datasource.ManageOpts{}); err != nil {
		log.DefaultLogger.Error("serve plugin", "error", err)
		os.Exit(1)
	}
}
