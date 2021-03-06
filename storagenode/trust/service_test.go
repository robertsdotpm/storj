// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package trust_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"storj.io/storj/internal/testcontext"
	"storj.io/storj/internal/testplanet"
)

func TestGetSignee(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	planet, err := testplanet.New(t, 1, 1, 0)
	require.NoError(t, err)
	defer ctx.Check(planet.Shutdown)

	planet.Start(ctx)

	trust := planet.StorageNodes[0].Storage2.Trust

	canceledContext, cancel := context.WithCancel(ctx)
	cancel()

	var group errgroup.Group
	group.Go(func() error {
		cert, err := trust.GetSignee(canceledContext, planet.Satellites[0].ID())
		if err != context.Canceled {
			return nil
		}
		if err != nil {
			return err
		}
		if cert != nil {
			return errors.New("got certificate")
		}
		return nil
	})

	group.Go(func() error {
		cert, err := trust.GetSignee(ctx, planet.Satellites[0].ID())
		if err != nil {
			return err
		}
		if cert == nil {
			return errors.New("didn't get certificate")
		}
		return nil
	})

	assert.NoError(t, group.Wait())
}
