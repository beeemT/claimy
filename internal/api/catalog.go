package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	catalogCreate struct {
		CanonicalName string `json:"canonicalName"`
	}
	catalogUpdate struct {
		sqlh.InputById[string]
		CanonicalName string `json:"canonicalName"`
	}
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

func groupDefinition() sqlh.CrudDefinition[uint64, catalogGroup, string, catalogCreate, catalogUpdate, appListInput, client.CatalogGroup] {
	d := sqlh.CrudDefinition[uint64, catalogGroup, string, catalogCreate, catalogUpdate, appListInput, client.CatalogGroup]{
		CreateInput: func(_ context.Context, in *catalogCreate) (*catalogGroup, error) {
			return &catalogGroup{CanonicalName: in.CanonicalName}, nil
		},
		UpdateInput: func(_ context.Context, e *catalogGroup, in *catalogUpdate) (*catalogGroup, error) {
			e.CanonicalName = in.CanonicalName

			return e, nil
		},
		PatchInputFromEntity: func(_ context.Context, e *catalogGroup) (*catalogUpdate, error) {
			return &catalogUpdate{InputById: sqlh.InputById[string]{Id: fmt.Sprint(e.Id)}, CanonicalName: e.CanonicalName}, nil
		},
		Output: func(_ context.Context, e *catalogGroup) (client.CatalogGroup, error) {
			return client.CatalogGroup{Id: fmt.Sprint(e.Id), CanonicalName: e.CanonicalName, CreatedAt: e.CreatedAt}, nil
		},
	}
	d.Identity = lookupCatalogGroup
	d.ListOperation = listCatalogGroups

	return d
}

func appOutput(_ context.Context, e *catalogApp) (client.CatalogApp, error) {
	return client.CatalogApp{Id: fmt.Sprint(e.Id), GroupId: fmt.Sprint(e.GroupID), CanonicalName: e.CanonicalName, CreatedAt: e.CreatedAt}, nil
}

func appDefinition() sqlh.CrudDefinition[uint64, catalogApp, string, catalogCreate, catalogUpdate, appListInput, client.CatalogApp] {
	d := sqlh.CrudDefinition[uint64, catalogApp, string, catalogCreate, catalogUpdate, appListInput, client.CatalogApp]{
		CreateInput: func(_ context.Context, in *catalogCreate) (*catalogApp, error) {
			return &catalogApp{CanonicalName: in.CanonicalName}, nil
		},
		UpdateInput: func(_ context.Context, e *catalogApp, in *catalogUpdate) (*catalogApp, error) {
			e.CanonicalName = in.CanonicalName

			return e, nil
		},
		PatchInputFromEntity: func(_ context.Context, e *catalogApp) (*catalogUpdate, error) {
			return &catalogUpdate{InputById: sqlh.InputById[string]{Id: fmt.Sprint(e.Id)}, CanonicalName: e.CanonicalName}, nil
		},
		Output: appOutput,
	}
	d.Identity = lookupCatalogApp
	d.ListOperation = listCatalogApps

	return d
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

func lookupCatalogApp(_ context.Context, tx sqlr.TTx, repo sqlr.RepositoryTx[uint64, catalogApp], id string, _ sqlh.QueryScope, builder func(*sqlr.QueryBuilderSelect)) (*catalogApp, error) {
	rows, err := repo.Query(tx, func(q *sqlr.QueryBuilderSelect) {
		builder(q)
		q.Where("canonical_name = ?", id)
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, claims.NewError(claims.NotFound, "app not found")
	}

	return &rows[0], nil
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

func registerCatalog(ctx context.Context, config cfg.Config, logger log.Logger, router *httpserver.Router, sqlClient sqlc.Client, identities auth.Authenticator) error {
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
	groupHandler := sqlh.NewCrudHandler(
		sqlh.SimpleCrudDefinition(groupDefinition()),
		sqlh.WithRepositorySettings[uint64, catalogGroup](sqlr.DefaultSettings()),
	)
	router.HandleWith(httpserver.With(groupHandler, func(r *httpserver.Router, h *sqlh.CrudHandler[uint64, catalogGroup, string, catalogCreate, catalogUpdate, appListInput, client.CatalogGroup]) {
		g := r.Group("/v1/catalog")
		g.Use(authz)
		g.GET("/groups/:id", httpserver.Bind(h.Read, httpserver.NoBodyBinding{}))
		g.POST("/groups/query", httpserver.Bind(h.List))
	}))
	appHandler := sqlh.NewCrudHandler(
		sqlh.SimpleCrudDefinition(appDefinition()),
		sqlh.WithRepositorySettings[uint64, catalogApp](sqlr.DefaultSettings()),
	)
	router.HandleWith(httpserver.With(appHandler, func(r *httpserver.Router, h *sqlh.CrudHandler[uint64, catalogApp, string, catalogCreate, catalogUpdate, appListInput, client.CatalogApp]) {
		g := r.Group("/v1/catalog")
		g.Use(authz)
		g.POST("/groups/:group/apps/query", httpserver.Bind(h.List))
	}))
	_ = ctx
	_ = config
	_ = logger

	return nil
}
