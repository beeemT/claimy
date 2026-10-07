package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/beeemT/claimy/internal/auth"
	"github.com/beeemT/claimy/internal/claims"
	"github.com/beeemT/claimy/pkg/client"
	"github.com/gin-gonic/gin"
	"github.com/gosoline-project/httpserver"
	"github.com/gosoline-project/sqlc"
	"github.com/gosoline-project/sqlh"
	"github.com/gosoline-project/sqlr"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/log"
)

type catalogGroup struct {
	sqlr.Entity[uint64]
	CanonicalName string `db:"canonical_name"`
}

func (catalogGroup) TableName() string { return "app_groups" }

func (catalogGroup) GetUpdatedAt() time.Time { return time.Time{} }

type catalogApp struct {
	sqlr.Entity[uint64]
	GroupID       uint64 `db:"group_id"`
	CanonicalName string `db:"canonical_name"`
}

func (catalogApp) GetUpdatedAt() time.Time { return time.Time{} }

func (catalogApp) TableName() string { return "apps" }

type (
	catalogFilter struct {
		CanonicalName *string `json:"canonicalName,omitempty"`
	}
	appListInput struct {
		Filter *catalogFilter `json:"filter,omitempty"`
		Page   sqlh.ListPage  `json:"page,omitempty"`
		Group  string         `uri:"group" json:"-"`
	}
)

func (in appListInput) GetForceFilters() []sqlh.ForceFilter { return nil }
func (in appListInput) ApplyFilters(q *sqlr.QueryBuilderSelect) error {
	if in.Filter == nil || in.Filter.CanonicalName == nil {
		return nil
	}
	if strings.TrimSpace(*in.Filter.CanonicalName) == "" {
		return fmt.Errorf("canonicalName filter must not be empty")
	}
	q.Where("canonical_name = ?", *in.Filter.CanonicalName)

	return nil
}
func (appListInput) ApplyQueryModifiers(*sqlr.QueryBuilderSelect) {}
func (in appListInput) ApplyPagination(q *sqlr.QueryBuilderSelect) {
	limit := in.Page.Limit
	if limit == 0 {
		limit = 100
	}
	q.Limit(limit)
	if in.Page.Offset > 0 {
		q.Offset(in.Page.Offset)
	}
}

func (in appListInput) ValidatePagination() error {
	if in.Page.Limit < 0 || in.Page.Offset < 0 {
		return fmt.Errorf("pagination values must not be negative")
	}
	if in.Page.Offset > 0 && in.Page.Limit == 0 {
		return fmt.Errorf("offset requires a positive limit")
	}
	if in.Page.Limit > 100 {
		return fmt.Errorf("limit must be at most 100")
	}

	return nil
}

func groupOutput(_ context.Context, e *catalogGroup) (client.CatalogGroup, error) {
	return client.CatalogGroup{Id: fmt.Sprint(e.Id), CanonicalName: e.CanonicalName, CreatedAt: e.CreatedAt}, nil
}

func appOutput(_ context.Context, e *catalogApp) (client.CatalogApp, error) {
	return client.CatalogApp{Id: fmt.Sprint(e.Id), GroupId: fmt.Sprint(e.GroupID), CanonicalName: e.CanonicalName, CreatedAt: e.CreatedAt}, nil
}

func lookupCatalogGroup(_ context.Context, tx sqlr.TTx, repo sqlr.RepositoryTx[uint64, catalogGroup], id string, _ sqlh.QueryScope, builder func(*sqlr.QueryBuilderSelect)) (*catalogGroup, error) {
	rows, err := repo.Query(tx, func(q *sqlr.QueryBuilderSelect) {
		builder(q)
		q.Where("canonical_name = ?", id)
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, claims.NewError(claims.NotFound, "group not found")
	}

	return &rows[0], nil
}

func listCatalogGroups(_ context.Context, tx sqlr.TTx, repo sqlr.RepositoryTx[uint64, catalogGroup], in *appListInput) (sqlh.ListOutput[client.CatalogGroup], error) {
	var out sqlh.ListOutput[client.CatalogGroup]
	if in == nil {
		return out, claims.NewError(claims.Invalid, "list input is required")
	}
	if err := in.ValidatePagination(); err != nil {
		return out, claims.NewError(claims.Invalid, err.Error())
	}
	countQuery := sqlr.NewQueryBuilderSelect()
	if err := in.ApplyFilters(countQuery); err != nil {
		return out, claims.NewError(claims.Invalid, err.Error())
	}
	total, err := repo.Count(tx, countQuery)
	if err != nil {
		return out, err
	}
	var filterErr error
	rows, err := repo.Query(tx, func(q *sqlr.QueryBuilderSelect) {
		filterErr = in.ApplyFilters(q)
		if filterErr != nil {
			q.Where("1 = 0")

			return
		}
		in.ApplyPagination(q)
	})
	if err != nil {
		return out, err
	}
	if filterErr != nil {
		return out, claims.NewError(claims.Invalid, filterErr.Error())
	}
	out.Total = total
	out.Results = make([]client.CatalogGroup, 0, len(rows))
	for i := range rows {
		out.Results = append(out.Results, client.CatalogGroup{Id: fmt.Sprint(rows[i].Id), CanonicalName: rows[i].CanonicalName, CreatedAt: rows[i].CreatedAt})
	}

	return out, nil
}

func catalogGroupID(ctx context.Context, tx sqlr.TTx, name string) (uint64, error) {
	var groupID uint64
	if err := tx.Get(ctx, &groupID, "SELECT id FROM app_groups WHERE canonical_name = ?", name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, claims.NewError(claims.NotFound, "group not found")
		}

		return 0, claims.NewError(claims.StorageError, "catalog group lookup failed")
	}

	return groupID, nil
}

func catalogAppRows(tx sqlr.TTx, repo sqlr.RepositoryTx[uint64, catalogApp], groupID uint64, in *appListInput) (int, []catalogApp, error) {
	countQuery := sqlr.NewQueryBuilderSelect().Where("group_id = ?", groupID)
	if err := in.ApplyFilters(countQuery); err != nil {
		return 0, nil, claims.NewError(claims.Invalid, err.Error())
	}
	total, err := repo.Count(tx, countQuery)
	if err != nil {
		return 0, nil, err
	}
	var filterErr error
	rows, err := repo.Query(tx, func(q *sqlr.QueryBuilderSelect) {
		q.Where("group_id = ?", groupID)
		filterErr = in.ApplyFilters(q)
		if filterErr != nil {
			q.Where("1 = 0")

			return
		}
		in.ApplyQueryModifiers(q)
		in.ApplyPagination(q)
	})
	if err != nil {
		return 0, nil, err
	}
	if filterErr != nil {
		return 0, nil, claims.NewError(claims.Invalid, filterErr.Error())
	}

	return total, rows, nil
}

func listCatalogApps(ctx context.Context, tx sqlr.TTx, repo sqlr.RepositoryTx[uint64, catalogApp], in *appListInput) (sqlh.ListOutput[client.CatalogApp], error) {
	var out sqlh.ListOutput[client.CatalogApp]
	if in == nil || strings.TrimSpace(in.Group) == "" {
		return out, claims.NewError(claims.Invalid, "group is required")
	}
	groupID, err := catalogGroupID(ctx, tx, in.Group)
	if err != nil {
		return out, err
	}
	if err := in.ValidatePagination(); err != nil {
		return out, claims.NewError(claims.Invalid, err.Error())
	}
	total, rows, err := catalogAppRows(tx, repo, groupID, in)
	if err != nil {
		return out, err
	}
	out.Total = total
	out.Results = make([]client.CatalogApp, 0, len(rows))
	for i := range rows {
		result, err := appOutput(ctx, &rows[i])
		if err != nil {
			return out, err
		}
		out.Results = append(out.Results, result)
	}

	return out, nil
}

type catalogHandlers struct {
	groupRepository sqlr.RepositoryTx[uint64, catalogGroup]
	appRepository   sqlr.RepositoryTx[uint64, catalogApp]
	txRunner        *sqlh.TxRunner
	closeOnce       sync.Once
	closeErr        error
}

func newCatalogHandlers(client sqlc.Client) (*catalogHandlers, error) {
	groupRepository, err := sqlr.NewRepositoryTxWithSettings[uint64, catalogGroup](client, sqlr.DefaultSettings())
	if err != nil {
		return nil, fmt.Errorf("create catalog group repository: %w", err)
	}
	appRepository, err := sqlr.NewRepositoryTxWithSettings[uint64, catalogApp](client, sqlr.DefaultSettings())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create catalog app repository: %w", err), groupRepository.Close())
	}
	txRunner, err := sqlh.NewTxRunnerWithClient(client)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create catalog transaction runner: %w", err), appRepository.Close(), groupRepository.Close())
	}

	return &catalogHandlers{
		groupRepository: groupRepository,
		appRepository:   appRepository,
		txRunner:        txRunner,
	}, nil
}

func (h *catalogHandlers) Close() error {
	if h == nil {
		return nil
	}

	h.closeOnce.Do(func() {
		h.closeErr = errors.Join(h.groupRepository.Close(), h.appRepository.Close())
	})

	return h.closeErr
}

func (h *catalogHandlers) readGroup(ctx context.Context, input *sqlh.InputById[string]) (client.CatalogGroup, error) {
	return h.txRunner.RunValue(ctx, input, func(ctx context.Context, tx sqlr.TTx, input *sqlh.InputById[string]) (client.CatalogGroup, error) {
		if input == nil {
			return client.CatalogGroup{}, fmt.Errorf("read input is required")
		}
		group, err := lookupCatalogGroup(ctx, tx, h.groupRepository, input.Id, nil, func(*sqlr.QueryBuilderSelect) {})
		if err != nil {
			return client.CatalogGroup{}, fmt.Errorf("failed to read entity with id %v: %w", input.Id, err)
		}
		output, err := groupOutput(ctx, group)
		if err != nil {
			return client.CatalogGroup{}, fmt.Errorf("failed to transform read entity: %w", err)
		}

		return output, nil
	})
}

func (h *catalogHandlers) listGroups(ctx context.Context, input *appListInput) (sqlh.ListOutput[client.CatalogGroup], error) {
	return h.txRunner.RunValue(ctx, input, func(ctx context.Context, tx sqlr.TTx, input *appListInput) (sqlh.ListOutput[client.CatalogGroup], error) {
		return listCatalogGroups(ctx, tx, h.groupRepository, input)
	})
}

func (h *catalogHandlers) listApps(ctx context.Context, input *appListInput) (sqlh.ListOutput[client.CatalogApp], error) {
	return h.txRunner.RunValue(ctx, input, func(ctx context.Context, tx sqlr.TTx, input *appListInput) (sqlh.ListOutput[client.CatalogApp], error) {
		return listCatalogApps(ctx, tx, h.appRepository, input)
	})
}

func registerCatalog(router *httpserver.Router, sqlClient sqlc.Client, identities auth.Authenticator) error {
	if sqlClient == nil {
		return fmt.Errorf("catalog: client is required")
	}
	authz := func(c *gin.Context) {
		h, ok := (&service{identities: identities}).actor(c)
		if !ok {
			c.Abort()

			return
		}
		c.Set("claimy.actor", h)
		c.Next()
	}
	handlerFactory := func(_ context.Context, _ cfg.Config, _ log.Logger) (*catalogHandlers, error) {
		return newCatalogHandlers(sqlClient)
	}
	router.HandleWith(httpserver.With(handlerFactory, func(r *httpserver.Router, h *catalogHandlers) {
		g := r.Group("/v1/catalog")
		g.Use(authz)
		g.GET("/groups/:id", httpserver.Bind(h.readGroup, httpserver.NoBodyBinding{}))
		g.POST("/groups/query", httpserver.Bind(h.listGroups))
		g.POST("/groups/:group/apps/query", httpserver.Bind(h.listApps))
	}))

	return nil
}
