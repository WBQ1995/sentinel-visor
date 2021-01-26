package commands

import (
	"context"
	"github.com/filecoin-project/sentinel-visor/chain"
	"github.com/filecoin-project/sentinel-visor/lens"
	"github.com/filecoin-project/sentinel-visor/storage"
	"github.com/filecoin-project/sentinel-visor/vector"
	"github.com/ipfs/go-cid"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type VectorContext struct {
	ctx context.Context

	cli *cli.Context

	from  int64
	to    int64
	tasks []string

	strg *storage.MemStorage

	opener lens.APIOpener
	closer lens.APICloser

	roots []cid.Cid

	// non-nil when validating.
	schema vector.RunnerSchema
}

var Vector = &cli.Command{
	Name:  "vector",
	Usage: "Vector tooling for Visor.",
	Subcommands: []*cli.Command{
		BuildVector,
		ExecuteVector,
	},
}

var BuildVector = &cli.Command{
	Name:  "build",
	Usage: "Create a vector.",
	Flags: []cli.Flag{
		&cli.Int64Flag{
			Name:    "from",
			Usage:   "Limit actor and message processing to tipsets at or above `HEIGHT`",
			EnvVars: []string{"VISOR_HEIGHT_FROM"},
		},
		&cli.Int64Flag{
			Name:        "to",
			Usage:       "Limit actor and message processing to tipsets at or below `HEIGHT`",
			Value:       estimateCurrentEpoch(),
			DefaultText: "MaxInt64",
			EnvVars:     []string{"VISOR_HEIGHT_TO"},
		},
		&cli.StringFlag{
			Name:    "tasks",
			Usage:   "Comma separated list of tasks to build. Each task is reported separately in the database.",
			Value:   strings.Join([]string{chain.BlocksTask}, ","),
			EnvVars: []string{"VISOR_WALK_TASKS"},
		},
		&cli.StringFlag{
			Name:  "vector-file",
			Usage: "Path of vector file.",
		},
	},
	Action: build,
}

func build(cctx *cli.Context) error {
	// Set up a context that is canceled when the command is interrupted
	ctx, cancel := context.WithCancel(cctx.Context)

	// Set up a signal handler to cancel the context
	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := setupLogging(cctx); err != nil {
		return xerrors.Errorf("setup logging: %w", err)
	}

	builder, err := vector.NewBuilder(cctx)
	if err != nil {
		return err
	}

	schema, err := builder.Build(ctx)
	if err != nil {
		return err
	}

	return schema.Persist(cctx.String("vector-file"))
}

var ExecuteVector = &cli.Command{
	Name:  "execute",
	Usage: "execute a test vector",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "vector-file",
			Usage:    "Path to vector file.",
			Required: true,
		},
	},
	Action: execute,
}

func execute(cctx *cli.Context) error {
	// Set up a context that is canceled when the command is interrupted
	ctx, cancel := context.WithCancel(cctx.Context)

	// Set up a signal handler to cancel the context
	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := setupLogging(cctx); err != nil {
		return xerrors.Errorf("setup logging: %w", err)
	}
	runner, err := vector.NewRunner(cctx)
	if err != nil {
		return err
	}

	err = runner.Run(ctx)
	if err != nil {
		return err
	}

	return runner.Validate(ctx)
}
