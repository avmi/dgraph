// +build !oss

/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Dgraph Community License (the "License"); you
 * may not use this file except in compliance with the License. You
 * may obtain a copy of the License at
 *
 *     https://github.com/dgraph-io/dgraph/blob/master/licenses/DCL.txt
 */

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/golang/glog"
)

// Processor handles the different stages of the backup process.
type Processor struct {
	// DB is the Badger pstore managed by this node.
	DB *badger.DB
	// Request stores the backup request containing the parameters for this backup.
	Request *pb.BackupRequest
	// Since indicates the beginning timestamp from which the backup should start.
	// For a partial backup, the value is the largest value from the previous manifest
	// files. For a full backup, Since is set to zero so that all data is included.
	Since uint64
}

// Manifest records backup details, these are values used during restore.
// Since is the timestamp from which the next incremental backup should start (it's set
// to the readTs of the current backup).
// Groups are the IDs of the groups involved.
type Manifest struct {
	sync.Mutex
	Since  uint64   `json:"since"`
	Groups []uint32 `json:"groups"`
}

// RunBackup uses the request values to create a stream writer then hand off the data
// retrieval to stream.Orchestrate. The writer will create all the fd's needed to
// collect the data and later move to the target.
// Returns errors on failure, nil on success.
func (r *Processor) RunBackup(ctx context.Context) (*pb.BackupResponse, error) {
	var emptyRes pb.BackupResponse

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	uri, err := url.Parse(r.Request.Destination)
	if err != nil {
		return &emptyRes, err
	}

	handler, err := r.newHandler(uri)
	if err != nil {
		return &emptyRes, err
	}

	if err := handler.SetupBackup(uri, r); err != nil {
		return &emptyRes, err
	}

	glog.V(3).Infof("Backup manifest version: %d", r.Since)

	stream := r.DB.NewStreamAt(r.Request.ReadTs)
	stream.LogPrefix = "Dgraph.Backup"
	newSince, err := stream.Backup(handler, r.Since)

	if err != nil {
		glog.Errorf("While taking backup: %v", err)
		return &emptyRes, err
	}

	if newSince > r.Request.ReadTs {
		glog.Errorf("Max timestamp seen during backup (%d) is greater than readTs (%d)",
			newSince, r.Request.ReadTs)
	}

	glog.V(2).Infof("Backup group %d version: %d", r.Request.GroupId, r.Request.ReadTs)
	if err = handler.Close(); err != nil {
		glog.Errorf("While closing handler: %v", err)
		return &emptyRes, err
	}
	glog.Infof("Backup complete: group %d at %d", r.Request.GroupId, r.Request.ReadTs)
	return &emptyRes, nil
}

// Complete will finalize a backup by writing the manifest at the backup destination.
func (r *Processor) Complete(ctx context.Context, manifest *Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	uri, err := url.Parse(r.Request.Destination)
	if err != nil {
		return err
	}

	handler, err := r.newHandler(uri)
	if err != nil {
		return err
	}

	if err := handler.CompleteBackup(uri, r, manifest); err != nil {
		return err
	}

	if err = json.NewEncoder(handler).Encode(manifest); err != nil {
		return err
	}

	if err = handler.Close(); err != nil {
		return err
	}
	glog.Infof("Backup completed OK.")
	return nil
}

// GoString implements the GoStringer interface for Manifest.
func (m *Manifest) GoString() string {
	return fmt.Sprintf(`Manifest{Since: %d, Groups: %v}`, m.Since, m.Groups)
}
