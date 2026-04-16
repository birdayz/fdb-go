package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func main() {
	rawKey := "mgn_loadtest_6node_benchmark_key_00000000000000000000000000000000"
	fdb.MustAPIVersion(720)
	cf := os.Getenv("FDB_CLUSTER_FILE")
	var fdbDB fdb.Database
	if cf != "" {
		fdbDB, _ = fdb.OpenDatabase(cf)
	} else {
		fdbDB = fdb.MustOpenDefault()
	}
	recordDB := rl.NewFDBDatabase(fdbDB)
	db, err := storage.NewDB(recordDB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])
	record := &storev1.ApiKey{
		Id:        proto.String("ak-loadtest"),
		Name:      proto.String("Load Test"),
		KeyHash:   proto.String(keyHash),
		KeyPrefix: proto.String(rawKey[:12] + "..."),
		CreatedAt: proto.Int64(time.Now().UnixMilli()),
		Revoked:   proto.Bool(false),
	}
	if err := db.ApiKeys().Create(context.Background(), record); err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
	}
	fmt.Println(rawKey)
}
