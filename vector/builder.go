package vector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/sentinel-visor/chain"
	"github.com/filecoin-project/sentinel-visor/lens"
	"github.com/filecoin-project/sentinel-visor/lens/camera"
	"github.com/filecoin-project/sentinel-visor/storage"
	logging "github.com/ipfs/go-log/v2"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
	"os"
	"strings"
	"time"
)

var log = logging.Logger("vector")

type BuilderSchema struct {
	Meta   Metadata           `json:"metadata"`
	Params Parameters         `json:"parameters"`
	CAR    Base64EncodedBytes `json:"car"`
	Exp    BuilderExpected    `json:"expected"`
}

func (bs *BuilderSchema) Persist(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	return json.NewEncoder(f).Encode(bs)
}

type Builder struct {
	From  int64
	To    int64
	Tasks []string

	storage *storage.MemStorage

	opener lens.APIOpener
	closer lens.APICloser
}

func NewBuilder(cctx *cli.Context) (*Builder, error) {
	from := cctx.Int64("from")
	to := cctx.Int64("to")
	if from > to {
		return nil, xerrors.Errorf("--from must not be greater than --to")
	}

	lensOpener, lensCloser, err := camera.NewAPIOpener(cctx)
	if err != nil {
		return nil, xerrors.Errorf("setup lens: %w", err)
	}

	return &Builder{
		From:    from,
		To:      to,
		Tasks:   strings.Split(cctx.String("tasks"), ","),
		storage: storage.NewMemStorage(),
		opener:  lensOpener,
		closer:  lensCloser,
	}, nil
}

func (b *Builder) Build(ctx context.Context) (*BuilderSchema, error) {
	// get root CIDs of vector (for the car file header).
	node, closer, err := b.opener.Open(ctx)
	if err != nil {
		return nil, xerrors.Errorf("open lens: %w", err)
	}
	defer closer()

	roots, err := node.ChainGetTipSetByHeight(ctx, abi.ChainEpoch(b.To), types.EmptyTSK)
	if err != nil {
		return nil, err
	}

	// perform a walk over the chain.
	tsIndexer, err := chain.NewTipSetIndexer(b.opener, b.storage, 0, "build_vector", b.Tasks)
	if err != nil {
		return nil, xerrors.Errorf("setup indexer: %w", err)
	}
	defer func() {
		if err := tsIndexer.Close(); err != nil {
			log.Errorw("failed to close tipset indexer cleanly", "error", err)
		}
	}()

	if err := chain.NewWalker(tsIndexer, b.opener, b.From, b.To).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return nil, err
	}

	// persist the chain data read during the above walk to `out`
	out := new(bytes.Buffer)
	gw := gzip.NewWriter(out)
	cameraOpener, ok := b.opener.(*camera.APIOpener)
	if !ok {
		panic("developer error")
	}
	if err := cameraOpener.CaptureAsCAR(ctx, gw, roots.Cids()...); err != nil {
		return nil, err
	}

	if err := gw.Flush(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}

	return &BuilderSchema{
		Meta: Metadata{
			Commit:      "TODO",
			Version:     "TODO",
			Description: "TODO",
			Network:     "TODO",
			Date:        time.Now().UTC().Unix(),
		},
		Params: Parameters{
			From:  b.From,
			To:    b.To,
			Tasks: b.Tasks,
		},
		CAR: out.Bytes(),
		Exp: BuilderExpected{
			Models: b.storage.Data,
		},
	}, nil
}
