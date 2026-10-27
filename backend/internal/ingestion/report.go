package ingestion

import (
	"context"

	"github.com/cytusc/node_list/backend/internal/domain"
)

type reportingImporter struct {
	*Importer
	report func(error)
}

func (i *reportingImporter) Import(ctx context.Context, snap domain.SourceBatch) error {
	err := i.Importer.Import(ctx, snap)
	i.report(err)
	return err
}
