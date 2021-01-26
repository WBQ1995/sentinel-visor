package vector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/blockstore"
	"github.com/filecoin-project/sentinel-visor/chain"
	"github.com/filecoin-project/sentinel-visor/lens"
	"github.com/filecoin-project/sentinel-visor/lens/util"
	"github.com/filecoin-project/sentinel-visor/model/blocks"
	"github.com/filecoin-project/sentinel-visor/storage"
	"github.com/google/go-cmp/cmp"
	"github.com/ipld/go-car"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
	"os"
)

type RunnerSchema struct {
	Meta   Metadata           `json:"metadata"`
	Params Parameters         `json:"parameters"`
	CAR    Base64EncodedBytes `json:"car"`
	Exp    RunnerExpected     `json:"expected"`
}

type Runner struct {
	schema RunnerSchema

	storage *storage.MemStorage

	opener lens.APIOpener
	closer lens.APICloser
}

func NewRunner(cctx *cli.Context) (*Runner, error) {
	vectorPath := cctx.String("vector-file")
	fecVile, err := os.OpenFile(vectorPath, os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	var vs RunnerSchema
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

	return &Runner{
		schema:  vs,
		storage: storage.NewMemStorage(),
		opener:  opener,
		closer:  closer,
	}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	tsIndexer, err := chain.NewTipSetIndexer(r.opener, r.storage, 0, "run_vector", r.schema.Params.Tasks)
	if err != nil {
		return xerrors.Errorf("setup indexer: %w", err)
	}
	defer func() {
		if err := tsIndexer.Close(); err != nil {
			log.Errorw("failed to close tipset indexer cleanly", "error", err)
		}
	}()

	if err := chain.NewWalker(tsIndexer, r.opener, r.schema.Params.From, r.schema.Params.To).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (r *Runner) Validate(ctx context.Context) error {
	actual := r.storage.Data
	expected := r.schema.Exp.Models

	// TODO there is a bug here, getting inconsistent results from the cmp.Diff call.
	for expTable, expData := range expected {
		log.Infow("Validating Model", "name", expTable)
		actData, ok := actual[expTable]
		if !ok {
			return xerrors.Errorf("Missing Table: %s", expTable)
		}

		err := modelTypeFromTable(expTable, expData, actData)
		if err != nil {
			return err
		}
	}
	return nil
}

func modelTypeFromTable(tableName string, expected json.RawMessage, actual []interface{}) error {
	// TODO: something with reflection
	switch tableName {
	case "block_headers":
		var expType blocks.BlockHeaders
		if err := json.Unmarshal(expected, &expType); err != nil {
			return err
		}

		var actType blocks.BlockHeaders
		for _, raw := range actual {
			actualBlock, ok := raw.(*blocks.BlockHeader)
			if !ok {
				panic("developer error")
			}
			actType = append(actType, actualBlock)
		}

		diff := cmp.Diff(actType, expType)
		if diff != "" {
			log.Errorw("Vector Model Fail", "name", tableName)
			fmt.Println(diff)
		} else {
			log.Infow("Vector Model Pass", "name", tableName)
		}
	case "drand_block_entries":
		var expType blocks.DrandBlockEntries
		if err := json.Unmarshal(expected, &expType); err != nil {
			return err
		}

		var actType blocks.DrandBlockEntries
		for _, raw := range actual {
			actualBlock, ok := raw.(*blocks.DrandBlockEntrie)
			if !ok {
				panic("developer error")
			}
			actType = append(actType, actualBlock)
		}

		diff := cmp.Diff(actType, expType)
		if diff != "" {
			log.Errorw("Vector Model Fail", "name", tableName)
			fmt.Println(diff)
		} else {
			log.Infow("Vector Model Pass", "name", tableName)
		}
	case "block_parents":
		var expType blocks.BlockParents
		if err := json.Unmarshal(expected, &expType); err != nil {
			return err
		}

		var actType blocks.BlockParents
		for _, raw := range actual {
			actualBlock, ok := raw.(*blocks.BlockParent)
			if !ok {
				panic("developer error")
			}
			actType = append(actType, actualBlock)
		}

		diff := cmp.Diff(actType, expType)
		if diff != "" {
			log.Errorw("Vector Model Fail", "name", tableName)
			fmt.Println(diff)
		} else {
			log.Infow("Vector Model Pass", "name", tableName)
		}
	}
	return nil
}
