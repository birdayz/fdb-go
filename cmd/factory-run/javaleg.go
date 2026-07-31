package main

import (
	"context"
	"fmt"

	"fdb.dev/pkg/relational/conformance/plandiff"
)

// javaLeg adapts the existing conformance-server client to the factory's
// CrossEngine interface.
//
// plandiff.SetupRunner is the entry the RFC names, and reusing it rather than
// speaking to the server directly is what keeps the factory's Java leg
// identical to the differential harness's: the same runWithSetup step, the
// same ephemeral-schema lifecycle, and the same JSON row encoding — so a
// divergence the factory reports means the same thing a plandiff divergence
// means.
type javaLeg struct {
	runner plandiff.SetupRunner
}

func newJavaLeg(baseURL, clusterFileContent string) *javaLeg {
	// The Go runner takes a cluster-file PATH (the driver opens it); the Java
	// runner takes the file's CONTENTS, which it ships over HTTP.
	r, ok := plandiff.NewJavaRunnerHTTP(baseURL, clusterFileContent).(plandiff.SetupRunner)
	if !ok {
		return nil
	}
	return &javaLeg{runner: r}
}

func (j *javaLeg) Rows(ctx context.Context, schemaTemplate string, setup []string, query string) ([][]any, error) {
	res := j.runner.RunWithSetup(ctx, schemaTemplate, setup, query)
	if res.Err != nil {
		return nil, fmt.Errorf("java runWithSetup: %w", res.Err)
	}
	return res.Rows.Rows, nil
}
