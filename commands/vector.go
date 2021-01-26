package commands

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/blockstore"
	"github.com/filecoin-project/sentinel-visor/lens"
	"github.com/filecoin-project/sentinel-visor/lens/camera"
	"github.com/filecoin-project/sentinel-visor/lens/util"
	"github.com/filecoin-project/sentinel-visor/model/blocks"
	"github.com/filecoin-project/sentinel-visor/vector"
	"github.com/google/go-cmp/cmp"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-car"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/filecoin-project/sentinel-visor/chain"
	"github.com/filecoin-project/sentinel-visor/storage"
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
	schema vector.OtherSchema
}

func (v *VectorContext) Close() {
	v.closer()
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
			Name:  "vector-dir",
			Usage: "Path to vector files.",
			Value: "./visor-vector",
		},
	},
	Action: build,
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

func validateAndSetupBuild(cctx *cli.Context) (*VectorContext, error) {
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

	// Validate flags
	heightFrom := cctx.Int64("from")
	heightTo := cctx.Int64("to")
	if heightFrom > heightTo {
		return nil, xerrors.Errorf("--from must not be greater than --to")
	}

	tasks := strings.Split(cctx.String("tasks"), ",")

	if err := setupLogging(cctx); err != nil {
		return nil, xerrors.Errorf("setup logging: %w", err)
	}

	lensOpener, lensCloser, err := camera.NewAPIOpener(cctx)
	if err != nil {
		return nil, xerrors.Errorf("setup lens: %w", err)
	}

	var strg = storage.NewMemStorage()

	// get vector root cids (for car file later)
	node, closer, err := lensOpener.Open(ctx)
	if err != nil {
		return nil, xerrors.Errorf("open lens: %w", err)
	}
	defer closer()

	root, err := node.ChainGetTipSetByHeight(ctx, abi.ChainEpoch(heightTo), types.EmptyTSK)
	if err != nil {
		return nil, err
	}

	return &VectorContext{
		ctx: ctx,
		cli: cctx,

		from:  heightFrom,
		to:    heightTo,
		tasks: tasks,

		strg: strg,

		opener: lensOpener,
		closer: lensCloser,

		roots: root.Cids(),
	}, nil
}

func validateAndSetupExecute(cctx *cli.Context) (*VectorContext, error) {
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
		return nil, xerrors.Errorf("setup logging: %w", err)
	}

	vectorPath := cctx.String("vector-file")
	fecVile, err := os.OpenFile(vectorPath, os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	var vs vector.OtherSchema
	if err := json.NewDecoder(fecVile).Decode(&vs); err != nil {
		return nil, err
	}
	// need to go from bytes representing a car file to a blockstore, then to a Lotus API.
	bs := blockstore.Blockstore(blockstore.NewTemporary())

	// Read the base64-encoded CAR from the vector, and inflate the gzip.
	buf := bytes.NewReader(vs.CAR)
	r, err := gzip.NewReader(buf)
	if err != nil {
		return nil, fmt.Errorf("failed to inflate gzipped CAR: %s", err)
	}
	defer r.Close()

	// put the thing in the blockstore.
	// TODO: the entire car file is now in memory, this is an unrealistic expectation, adjust at some point.
	carHeader, err := car.LoadCar(bs, r)
	if err != nil {
		return nil, fmt.Errorf("failed to load state tree car from test vector: %s", err)
	}

	cacheDB := util.NewCachingStore(bs)

	h := func(ctx context.Context, lookback int) (*types.TipSetKey, error) {
		tsk := types.NewTipSetKey(carHeader.Roots...)
		return &tsk, nil
	}

	opener, closer, err := util.NewAPIOpener(cctx, cacheDB, h)
	if err != nil {
		return nil, err
	}

	var strg = storage.NewMemStorage()
	return &VectorContext{
		ctx:    cctx.Context,
		cli:    cctx,
		from:   vs.Params.From,
		to:     vs.Params.To,
		tasks:  vs.Params.Tasks,
		strg:   strg,
		opener: opener,
		closer: closer,
		roots:  carHeader.Roots,
		schema: vs,
	}, nil

}

func execute(cctx *cli.Context) error {
	vctx, err := validateAndSetupExecute(cctx)
	if err != nil {
		return err
	}
	defer vctx.Close()

	tsIndexer, err := chain.NewTipSetIndexer(vctx.opener, vctx.strg, 0, cctx.String("name"), vctx.tasks)
	if err != nil {
		return xerrors.Errorf("setup indexer: %w", err)
	}
	defer func() {
		if err := tsIndexer.Close(); err != nil {
			log.Errorw("failed to close tipset indexer cleanly", "error", err)
		}
	}()

	walker := chain.NewWalker(tsIndexer, vctx.opener, vctx.from, vctx.to)

	err = walker.Run(vctx.ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return validateVector(vctx)
}

func validateVector(vec *VectorContext) error {
	actual := vec.strg.Data
	expected := vec.schema.Exp.Models

	for expTable, expData := range expected {
		actData, ok := actual[expTable]
		if !ok {
			return xerrors.Errorf("Missing Table: %s", expTable)
		}

		if expTable != "block_headers" {
			continue
		}

		var expectedBlocks blocks.BlockHeaders
		if err := json.Unmarshal(expData, &expectedBlocks); err != nil {
			return err
		}

		var actualBlocks blocks.BlockHeaders
		for _, raw := range actData {
			actualBlock, ok := raw.(*blocks.BlockHeader)
			if !ok {
				panic("developer error")
			}
			actualBlocks = append(actualBlocks, actualBlock)
		}

		diff := cmp.Diff(expectedBlocks, actualBlocks)
		if diff != "" {
			log.Error("Vector execution detected difference")
			fmt.Println(diff)
		}

	}
	return nil
}

func build(cctx *cli.Context) error {
	vctx, err := validateAndSetupBuild(cctx)
	if err != nil {
		return err
	}
	defer vctx.Close()

	tsIndexer, err := chain.NewTipSetIndexer(vctx.opener, vctx.strg, 0, cctx.String("name"), vctx.tasks)
	if err != nil {
		return xerrors.Errorf("setup indexer: %w", err)
	}
	defer func() {
		if err := tsIndexer.Close(); err != nil {
			log.Errorw("failed to close tipset indexer cleanly", "error", err)
		}
	}()
	walker := chain.NewWalker(tsIndexer, vctx.opener, vctx.from, vctx.to)

	err = walker.Run(vctx.ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return buildVector(vctx)
}

func buildVector(vec *VectorContext) error {
	var (
		out = new(bytes.Buffer)
		gw  = gzip.NewWriter(out)
	)

	cameraOpener, ok := vec.opener.(*camera.APIOpener)
	if !ok {
		panic("developer error")
	}
	if err := cameraOpener.CaptureAsCAR(vec.ctx, gw, vec.roots...); err != nil {
		return err
	}

	if err := gw.Flush(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	schema := vector.Schema{
		Meta: vector.SchemaMetadata{
			Commit:      "TODO",
			Version:     "TODO",
			Description: "TODO",
			Network:     "TODO",
			Date:        time.Now().UTC().Unix(),
		},
		Params: vector.Parameters{
			From:  vec.from,
			To:    vec.to,
			Tasks: vec.tasks,
		},
		CAR: out.Bytes(),
		Exp: vector.Expected{
			Models: vec.strg.Data,
		},
	}

	vectorPath := vec.cli.String("vector-dir")
	f, err := os.OpenFile(fmt.Sprintf("%s/vector.json", vectorPath), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(schema); err != nil {
		panic(err)
	}

	return nil
}
