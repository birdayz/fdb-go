package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// CheckHealth opens the cluster, runs SELECT 1 and, unless otelMode is off,
// reads the default schema's STORE_META layout (RFC §3). A failure names the
// step and the FDB error code or SQLSTATE.
func (d *Datasource) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if !d.gate.enter() {
		return healthFailure(stepSettings, errDisposed), nil
	}
	defer d.gate.leave()
	if d.cfgErr != nil {
		return healthFailure(stepSettings, d.cfgErr), nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.cfg.QueryTimeout)
	defer cancel()

	if _, err := d.database(ctx); err != nil {
		return healthFailure(stepOpenCluster, err), nil
	}

	schema := d.cfg.DefaultSchema
	where := d.cfg.Database
	if schema != "" {
		where += " schema " + schema
	}
	if err := d.selectOne(ctx, schema); err != nil {
		return healthFailure(stepSQL, err), nil
	}
	msg := fmt.Sprintf("FoundationDB: SELECT 1 ok in %s", where)

	switch {
	case d.cfg.OTelMode == otelModeOff:
		msg += "; OTel queries are off"
	case schema == "":
		msg += "; OTel layout not checked (no default schema)"
	default:
		r, release, err := d.readerFor(ctx, schema)
		var notOTel *NotOTelStoreError
		switch {
		case err == nil:
			msg += "; OTEL_V1 store, " + r.Layout()
			release()
		case d.cfg.OTelMode == otelModeAuto && errors.As(err, &notOTel):
			msg += "; the default schema is not an OTEL_V1 store, so only SQL queries apply to it"
		default:
			return healthFailure(stepOTel, err), nil
		}
	}
	return &backend.CheckHealthResult{Status: backend.HealthStatusOk, Message: msg}, nil
}

func (d *Datasource) selectOne(ctx context.Context, schema string) error {
	t, release, err := d.sqlPools.acquire(ctx, schema)
	if err != nil {
		return err
	}
	defer release()
	var one int64
	if err := t.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return fmt.Errorf("SELECT 1 returned %d", one)
	}
	return nil
}

func healthFailure(step string, err error) *backend.CheckHealthResult {
	var se *StepError
	if errors.As(err, &se) {
		step, err = se.Step, se.Err
	}
	details, _ := json.Marshal(map[string]string{"step": step, "code": errorCode(err), "error": err.Error()})
	return &backend.CheckHealthResult{
		Status:      backend.HealthStatusError,
		Message:     fmt.Sprintf("%s failed: %s", step, describeError(err)),
		JSONDetails: details,
	}
}
