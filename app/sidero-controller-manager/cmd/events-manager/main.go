// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/siderolabs/siderolink/api/events"
	sink "github.com/siderolabs/siderolink/pkg/events"

	"github.com/siderolabs/sidero/app/sidero-controller-manager/internal/siderolink"
)

var negativeAddressFilter []string

func main() {
	pflag.StringSliceVar(&negativeAddressFilter, "negative-address-filter", nil, "list of CIDR prefixes to filter out from the address events")
	pflag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s", err)
		os.Exit(1)
	}
}

func run() error {
	logger, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}

	zap.ReplaceGlobals(logger)
	zap.RedirectStdLog(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	negativeFilter := make([]netip.Prefix, 0, len(negativeAddressFilter))

	for _, prefixStr := range negativeAddressFilter {
		if prefixStr == "-" {
			continue
		}

		prefix, err := netip.ParsePrefix(prefixStr)
		if err != nil {
			return err
		}

		negativeFilter = append(negativeFilter, prefix)
	}

	eg, ctx := errgroup.WithContext(ctx)

	address := fmt.Sprintf(":%d", siderolink.EventsSinkPort)

	lis, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("error listening for gRPC API: %w", err)
	}

	s := grpc.NewServer()

	client, kubeconfig, err := getMetalClient()
	if err != nil {
		return fmt.Errorf("error getting metal client: %w", err)
	}

	annotator := siderolink.NewAnnotator(client, kubeconfig, logger)

	adapter := NewAdapter(client,
		annotator,
		logger.With(zap.String("component", "sink")),
		negativeFilter,
	)

	srv := sink.NewSink(adapter,
		[]protoreflect.ProtoMessage{
			&machine.AddressEvent{},
			&machine.ConfigValidationErrorEvent{},
			&machine.ConfigLoadErrorEvent{},
			&machine.PhaseEvent{},
			&machine.TaskEvent{},
			&machine.ServiceStateEvent{},
			&machine.SequenceEvent{},
		},
	)

	events.RegisterEventSinkServiceServer(s, srv)

	eg.Go(func() error {
		return annotator.Run(ctx)
	})

	eg.Go(func() error {
		logger.Info("started gRPC event sink", zap.String("address", address))

		return s.Serve(lis)
	})

	eg.Go(func() error {
		<-ctx.Done()

		s.Stop()

		return nil
	})

	if err := eg.Wait(); err != nil && !errors.Is(err, grpc.ErrServerStopped) && errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}
