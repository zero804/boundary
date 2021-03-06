package iam

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/hashicorp/boundary/internal/db"
	dbcommon "github.com/hashicorp/boundary/internal/db/common"
	"github.com/hashicorp/boundary/internal/errors"
	"github.com/hashicorp/boundary/internal/kms"
	"github.com/hashicorp/boundary/internal/oplog"
	"github.com/hashicorp/boundary/internal/types/resource"
	"github.com/hashicorp/boundary/internal/types/scope"
	wrapping "github.com/hashicorp/go-kms-wrapping"
)

// CreateScope will create a scope in the repository and return the written
// scope. Supported options include: WithPublicId and WithRandomReader.
func (r *Repository) CreateScope(ctx context.Context, s *Scope, userId string, opt ...Option) (*Scope, error) {
	if s == nil {
		return nil, fmt.Errorf("create scope: missing scope %w", errors.ErrInvalidParameter)
	}
	if s.Scope == nil {
		return nil, fmt.Errorf("create scope: missing scope store %w", errors.ErrInvalidParameter)
	}
	if s.PublicId != "" {
		return nil, fmt.Errorf("create scope: public id not empty: %w", errors.ErrInvalidParameter)
	}

	var parentOplogWrapper wrapping.Wrapper
	var externalWrappers *kms.ExternalWrappers
	var err error
	switch s.Type {
	case scope.Unknown.String():
		return nil, fmt.Errorf("create scope: unknown type: %w", errors.ErrInvalidParameter)
	case scope.Global.String():
		return nil, fmt.Errorf("create scope: invalid type: %w", errors.ErrInvalidParameter)
	default:
		switch s.ParentId {
		case "":
			return nil, fmt.Errorf("create scope: missing parent id: %w", errors.ErrInvalidParameter)
		case scope.Global.String():
			parentOplogWrapper, err = r.kms.GetWrapper(ctx, scope.Global.String(), kms.KeyPurposeOplog)
		default:
			parentOplogWrapper, err = r.kms.GetWrapper(ctx, s.ParentId, kms.KeyPurposeOplog)
		}
		externalWrappers = r.kms.GetExternalWrappers()
	}
	if err != nil {
		return nil, fmt.Errorf("create scope: unable to get oplog wrapper: %w", err)
	}

	opts := getOpts(opt...)

	var scopePublicId string
	var scopeMetadata oplog.Metadata
	var scopeRaw interface{}
	{
		scopeType := scope.Map[s.Type]
		if opts.withPublicId != "" {
			if !strings.HasPrefix(opts.withPublicId, scopeType.Prefix()+"_") {
				return nil, fmt.Errorf("create scope: passed-in public ID %q has wrong prefix for type %q which uses prefix %q", opts.withPublicId, scopeType.String(), scopeType.Prefix())
			}
			scopePublicId = opts.withPublicId
		} else {
			scopePublicId, err = newScopeId(scopeType)
			if err != nil {
				return nil, fmt.Errorf("create scope: error generating public id for new scope: %w", err)
			}
		}
		sc := s.Clone().(*Scope)
		sc.PublicId = scopePublicId
		scopeRaw = sc
		scopeMetadata, err = r.stdMetadata(ctx, sc)
		if err != nil {
			return nil, fmt.Errorf("create scope: error getting metadata for scope create: %w", err)
		}
		scopeMetadata["op-type"] = []string{oplog.OpType_OP_TYPE_CREATE.String()}
	}

	var adminRolePublicId string
	var adminRoleMetadata oplog.Metadata
	var adminRole *Role
	var adminRoleRaw interface{}
	switch {
	case userId == "",
		userId == "u_anon",
		userId == "u_auth",
		userId == "u_recovery",
		opts.withSkipAdminRoleCreation:
		// TODO: Cause a log entry. The repo doesn't have a logger right now,
		// and ideally we will be using context to pass around log info scoped
		// to this request for grouped display in the server log. The only
		// reason this should ever happen anyways is via the administrative
		// recovery workflow so it's already a special case.

		// Also, stop linter from complaining
		_ = adminRole

	default:
		adminRole, err = NewRole(scopePublicId)
		if err != nil {
			return nil, fmt.Errorf("create scope: error instantiating new admin role: %w", err)
		}
		adminRolePublicId, err = newRoleId()
		if err != nil {
			return nil, fmt.Errorf("create scope: error generating public id for new admin role: %w", err)
		}
		adminRole.PublicId = adminRolePublicId
		adminRole.Name = "Administration"
		adminRole.Description = fmt.Sprintf("Role created for administration of scope %s by user %s at its creation time", scopePublicId, userId)
		adminRoleRaw = adminRole
		adminRoleMetadata = oplog.Metadata{
			"resource-public-id": []string{adminRolePublicId},
			"scope-id":           []string{scopePublicId},
			"scope-type":         []string{s.Type},
			"resource-type":      []string{resource.Role.String()},
			"op-type":            []string{oplog.OpType_OP_TYPE_CREATE.String()},
		}
	}

	var defaultRolePublicId string
	var defaultRoleMetadata oplog.Metadata
	var defaultRole *Role
	var defaultRoleRaw interface{}
	if !opts.withSkipDefaultRoleCreation && s.Type == scope.Org.String() {
		defaultRole, err = NewRole(scopePublicId)
		if err != nil {
			return nil, fmt.Errorf("create scope: error instantiating new default role: %w", err)
		}
		defaultRolePublicId, err = newRoleId()
		if err != nil {
			return nil, fmt.Errorf("create scope: error generating public id for new default role: %w", err)
		}
		defaultRole.PublicId = defaultRolePublicId
		defaultRole.Name = "Login and Default Grants"
		defaultRole.Description = fmt.Sprintf("Role created for login capability and account self-management for users of scope %s at its creation time", scopePublicId)
		defaultRoleRaw = defaultRole
		defaultRoleMetadata = oplog.Metadata{
			"resource-public-id": []string{defaultRolePublicId},
			"scope-id":           []string{scopePublicId},
			"scope-type":         []string{s.Type},
			"resource-type":      []string{resource.Role.String()},
			"op-type":            []string{oplog.OpType_OP_TYPE_CREATE.String()},
		}
	}

	reader := opts.withRandomReader
	if reader == nil {
		reader = rand.Reader
	}

	_, err = r.writer.DoTx(
		ctx,
		db.StdRetryCnt,
		db.ExpBackoff{},
		func(dbr db.Reader, w db.Writer) error {
			if err := w.Create(
				ctx,
				scopeRaw,
				db.WithOplog(parentOplogWrapper, scopeMetadata),
			); err != nil {
				return fmt.Errorf("error creating scope: %w", err)
			}

			s := scopeRaw.(*Scope)

			// Create the scope's keys
			_, err = kms.CreateKeysTx(ctx, dbr, w, externalWrappers.Root(), reader, s.PublicId)
			if err != nil {
				return fmt.Errorf("error creating scope keys: %w", err)
			}

			kmsRepo, err := kms.NewRepository(dbr, w)
			if err != nil {
				return fmt.Errorf("error creating new kms repo: %w", err)
			}
			childOplogWrapper, err := r.kms.GetWrapper(ctx, s.PublicId, kms.KeyPurposeOplog, kms.WithRepository(kmsRepo))
			if err != nil {
				return fmt.Errorf("error fetching new scope oplog wrapper: %w", err)
			}

			// We create a new role, then set grants and principals on it. This
			// turns into a bunch of stuff sadly because the role is the
			// aggregate.
			if adminRoleRaw != nil {
				if err := w.Create(
					ctx,
					adminRoleRaw,
					db.WithOplog(childOplogWrapper, adminRoleMetadata),
				); err != nil {
					return fmt.Errorf("error creating role: %w", err)
				}

				adminRole = adminRoleRaw.(*Role)

				msgs := make([]*oplog.Message, 0, 3)
				roleTicket, err := w.GetTicket(adminRole)
				if err != nil {
					return fmt.Errorf("unable to get ticket: %w", err)
				}

				// We need to update the role version as that's the aggregate
				var roleOplogMsg oplog.Message
				rowsUpdated, err := w.Update(ctx, adminRole, []string{"Version"}, nil, db.NewOplogMsg(&roleOplogMsg), db.WithVersion(&adminRole.Version))
				if err != nil {
					return fmt.Errorf("unable to update role version for adding grant: %w", err)
				}
				if rowsUpdated != 1 {
					return fmt.Errorf("updated role but %d rows updated", rowsUpdated)
				}

				msgs = append(msgs, &roleOplogMsg)

				roleGrant, err := NewRoleGrant(adminRolePublicId, "id=*;type=*;actions=*")
				if err != nil {
					return fmt.Errorf("unable to create in memory role grant: %w", err)
				}
				roleGrantOplogMsgs := make([]*oplog.Message, 0, 1)
				if err := w.CreateItems(ctx, []interface{}{roleGrant}, db.NewOplogMsgs(&roleGrantOplogMsgs)); err != nil {
					return fmt.Errorf("unable to add grants: %w", err)
				}
				msgs = append(msgs, roleGrantOplogMsgs...)

				rolePrincipal, err := NewUserRole(adminRolePublicId, userId)
				if err != nil {
					return fmt.Errorf("unable to create in memory role user: %w", err)
				}
				roleUserOplogMsgs := make([]*oplog.Message, 0, 1)
				if err := w.CreateItems(ctx, []interface{}{rolePrincipal}, db.NewOplogMsgs(&roleUserOplogMsgs)); err != nil {
					return fmt.Errorf("unable to add grants: %w", err)
				}
				msgs = append(msgs, roleUserOplogMsgs...)

				metadata := oplog.Metadata{
					"op-type":            []string{oplog.OpType_OP_TYPE_CREATE.String()},
					"scope-id":           []string{s.PublicId},
					"scope-type":         []string{s.Type},
					"resource-public-id": []string{adminRole.PublicId},
				}
				if err := w.WriteOplogEntryWith(ctx, childOplogWrapper, roleTicket, metadata, msgs); err != nil {
					return fmt.Errorf("unable to write oplog: %w", err)
				}
			}

			// We create a new role, then set grants and principals on it. This
			// turns into a bunch of stuff sadly because the role is the
			// aggregate.
			if defaultRoleRaw != nil {
				if err := w.Create(
					ctx,
					defaultRoleRaw,
					db.WithOplog(childOplogWrapper, defaultRoleMetadata),
				); err != nil {
					return fmt.Errorf("error creating role: %w", err)
				}

				defaultRole = defaultRoleRaw.(*Role)

				msgs := make([]*oplog.Message, 0, 6)
				roleTicket, err := w.GetTicket(defaultRole)
				if err != nil {
					return fmt.Errorf("unable to get ticket: %w", err)
				}

				// We need to update the role version as that's the aggregate
				var roleOplogMsg oplog.Message
				rowsUpdated, err := w.Update(ctx, defaultRole, []string{"Version"}, nil, db.NewOplogMsg(&roleOplogMsg), db.WithVersion(&defaultRole.Version))
				if err != nil {
					return fmt.Errorf("unable to update role version for adding grant: %w", err)
				}
				if rowsUpdated != 1 {
					return fmt.Errorf("updated role but %d rows updated", rowsUpdated)
				}
				msgs = append(msgs, &roleOplogMsg)

				// Grants
				{
					grants := []interface{}{}
					roleGrant, err := NewRoleGrant(defaultRolePublicId, "type=scope;actions=list")
					if err != nil {
						return fmt.Errorf("unable to create in memory role grant: %w", err)
					}
					grants = append(grants, roleGrant)

					roleGrant, err = NewRoleGrant(defaultRolePublicId, "id=*;type=auth-method;actions=authenticate,list")
					if err != nil {
						return fmt.Errorf("unable to create in memory role grant: %w", err)
					}
					grants = append(grants, roleGrant)
					roleGrant, err = NewRoleGrant(defaultRolePublicId, "id={{account.id}};actions=read,change-password")
					if err != nil {
						return fmt.Errorf("unable to create in memory role grant: %w", err)
					}
					grants = append(grants, roleGrant)

					roleGrantOplogMsgs := make([]*oplog.Message, 0, 3)
					if err := w.CreateItems(ctx, grants, db.NewOplogMsgs(&roleGrantOplogMsgs)); err != nil {
						return fmt.Errorf("unable to add grants: %w", err)
					}
					msgs = append(msgs, roleGrantOplogMsgs...)
				}

				// Principals
				{
					principals := []interface{}{}
					rolePrincipal, err := NewUserRole(defaultRolePublicId, "u_anon")
					if err != nil {
						return fmt.Errorf("unable to create in memory role user: %w", err)
					}
					principals = append(principals, rolePrincipal)

					roleUserOplogMsgs := make([]*oplog.Message, 0, 2)
					if err := w.CreateItems(ctx, principals, db.NewOplogMsgs(&roleUserOplogMsgs)); err != nil {
						return fmt.Errorf("unable to add grants: %w", err)
					}
					msgs = append(msgs, roleUserOplogMsgs...)
				}

				metadata := oplog.Metadata{
					"op-type":            []string{oplog.OpType_OP_TYPE_CREATE.String()},
					"scope-id":           []string{s.PublicId},
					"scope-type":         []string{s.Type},
					"resource-public-id": []string{defaultRole.PublicId},
				}
				if err := w.WriteOplogEntryWith(ctx, childOplogWrapper, roleTicket, metadata, msgs); err != nil {
					return fmt.Errorf("unable to write oplog: %w", err)
				}
			}

			return nil
		},
	)

	if err != nil {
		if errors.IsUniqueError(err) {
			return nil, fmt.Errorf("create scope: scope %s/%s already exists: %w", scopePublicId, s.Name, errors.ErrNotUnique)
		}
		return nil, fmt.Errorf("create scope: id %s got error: %w", scopePublicId, err)
	}
	return scopeRaw.(*Scope), nil
}

// UpdateScope will update a scope in the repository and return the written
// scope.  fieldMaskPaths provides field_mask.proto paths for fields that should
// be updated.  Fields will be set to NULL if the field is a zero value and
// included in fieldMask. Name and Description are the only updatable fields,
// and everything else is ignored.  If no updatable fields are included in the
// fieldMaskPaths, then an error is returned.
func (r *Repository) UpdateScope(ctx context.Context, scope *Scope, version uint32, fieldMaskPaths []string, opt ...Option) (*Scope, int, error) {
	if scope == nil {
		return nil, db.NoRowsAffected, fmt.Errorf("update scope: missing scope: %w", errors.ErrInvalidParameter)
	}
	if scope.PublicId == "" {
		return nil, db.NoRowsAffected, fmt.Errorf("update scope: missing public id: %w", errors.ErrInvalidParameter)
	}
	if contains(fieldMaskPaths, "ParentId") {
		return nil, db.NoRowsAffected, fmt.Errorf("update scope: you cannot change a scope's parent: %w", errors.ErrInvalidFieldMask)
	}
	var dbMask, nullFields []string
	dbMask, nullFields = dbcommon.BuildUpdatePaths(
		map[string]interface{}{
			"name":        scope.Name,
			"description": scope.Description,
		},
		fieldMaskPaths,
		nil,
	)
	// nada to update, so reload scope from db and return it
	if len(dbMask) == 0 && len(nullFields) == 0 {
		return nil, db.NoRowsAffected, fmt.Errorf("update scope: %w", errors.ErrEmptyFieldMask)
	}

	resource, rowsUpdated, err := r.update(ctx, scope, version, dbMask, nullFields)
	if err != nil {
		if errors.IsUniqueError(err) {
			return nil, db.NoRowsAffected, fmt.Errorf("update scope: %s name %s already exists: %w", scope.PublicId, scope.Name, errors.ErrNotUnique)
		}
		return nil, db.NoRowsAffected, fmt.Errorf("update scope: failed for public id %s: %w", scope.PublicId, err)
	}
	return resource.(*Scope), rowsUpdated, err
}

// LookupScope will look up a scope in the repository.  If the scope is not
// found, it will return nil, nil.
func (r *Repository) LookupScope(ctx context.Context, withPublicId string, opt ...Option) (*Scope, error) {
	if withPublicId == "" {
		return nil, fmt.Errorf("lookup scope: missing public id %w", errors.ErrInvalidParameter)
	}
	scope := allocScope()
	scope.PublicId = withPublicId
	if err := r.reader.LookupByPublicId(ctx, &scope); err != nil {
		if err == errors.ErrRecordNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup scope: failed %w fo %s", err, withPublicId)
	}
	return &scope, nil
}

// DeleteScope will delete a scope from the repository
func (r *Repository) DeleteScope(ctx context.Context, withPublicId string, opt ...Option) (int, error) {
	if withPublicId == "" {
		return db.NoRowsAffected, fmt.Errorf("delete scope: missing public id %w", errors.ErrInvalidParameter)
	}
	if withPublicId == scope.Global.String() {
		return db.NoRowsAffected, fmt.Errorf("delete scope: invalid to delete global scope: %w", errors.ErrInvalidParameter)
	}
	scope := allocScope()
	scope.PublicId = withPublicId
	rowsDeleted, err := r.delete(ctx, &scope)
	if err != nil {
		if errors.Is(err, ErrMetadataScopeNotFound) {
			return 0, nil
		}
		return db.NoRowsAffected, fmt.Errorf("delete scope: failed %w for %s", err, withPublicId)
	}
	return rowsDeleted, nil
}

// ListProjects in an org and supports the WithLimit option.
func (r *Repository) ListProjects(ctx context.Context, withOrgId string, opt ...Option) ([]*Scope, error) {
	if withOrgId == "" {
		return nil, fmt.Errorf("list projects: missing org id %w", errors.ErrInvalidParameter)
	}
	var projects []*Scope
	err := r.list(ctx, &projects, "parent_id = ? and type = ?", []interface{}{withOrgId, scope.Project.String()}, opt...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return projects, nil
}

// ListOrgs and supports the WithLimit option.
func (r *Repository) ListOrgs(ctx context.Context, opt ...Option) ([]*Scope, error) {
	var orgs []*Scope
	err := r.list(ctx, &orgs, "parent_id = ? and type = ?", []interface{}{"global", scope.Org.String()}, opt...)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	return orgs, nil
}
