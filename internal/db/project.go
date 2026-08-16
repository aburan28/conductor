package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

type CreateProjectParams struct {
	OrganizationID  domain.ID
	Slug            string
	DisplayName     string
	CanonicalRemote string
	DefaultBranch   string
	WorktreeRoot    string
	RepoPath        string
	Config          domain.ProjectConfig
}

func (s *Store) CreateProject(ctx context.Context, p CreateProjectParams) (domain.Project, error) {
	if p.DefaultBranch == "" {
		p.DefaultBranch = "main"
	}
	if p.WorktreeRoot == "" {
		p.WorktreeRoot = ".conductor/runtime/worktrees"
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Slug
	}
	body, err := marshalJSON(p.Config)
	if err != nil {
		return domain.Project{}, err
	}
	sum := sha256.Sum256(body)

	var out domain.Project
	var raw []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO projects (organization_id, slug, display_name, canonical_remote,
		                      default_branch, worktree_root, repo_path, config, config_sha)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+projectColumnsBare,
		p.OrganizationID, p.Slug, p.DisplayName, p.CanonicalRemote,
		p.DefaultBranch, p.WorktreeRoot, p.RepoPath, body, hex.EncodeToString(sum[:]),
	).Scan(&out.ID, &out.OrganizationID, &out.Slug, &out.DisplayName, &out.CanonicalRemote,
		&out.DefaultBranch, &out.WorktreeRoot, &out.RepoPath, &raw, &out.ConfigSHA,
		&out.WorkflowSHA, &out.CreatedAt)
	if err != nil {
		return domain.Project{}, err
	}
	out.Config = domain.DefaultProjectConfig()
	if err := decodeJSON(raw, &out.Config); err != nil {
		return domain.Project{}, err
	}
	return out, nil
}

// projectColumns is table-qualified because ListProjectsFor joins project_memberships, which
// also has created_at and project_id. An unqualified list is ambiguous there and Postgres
// rejects the whole query.
const projectColumns = `
	p.id::text, p.organization_id::text, p.slug, p.display_name, p.canonical_remote,
	p.default_branch, p.worktree_root, p.repo_path, p.config, p.config_sha,
	p.workflow_sha, p.created_at`

// projectColumnsBare is the same list for RETURNING clauses, which cannot use a table alias.
var projectColumnsBare = strings.ReplaceAll(projectColumns, "p.", "")

func scanProject(scan func(...any) error) (domain.Project, error) {
	var p domain.Project
	var raw []byte
	if err := scan(&p.ID, &p.OrganizationID, &p.Slug, &p.DisplayName, &p.CanonicalRemote,
		&p.DefaultBranch, &p.WorktreeRoot, &p.RepoPath, &raw, &p.ConfigSHA,
		&p.WorkflowSHA, &p.CreatedAt); err != nil {
		return domain.Project{}, err
	}
	// Start from defaults so a config written by an older build still yields a complete,
	// working policy rather than zero-valued lease TTLs.
	p.Config = domain.DefaultProjectConfig()
	if err := decodeJSON(raw, &p.Config); err != nil {
		return domain.Project{}, err
	}
	return p, nil
}

func (s *Store) GetProject(ctx context.Context, id domain.ID) (domain.Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx,
		`SELECT `+projectColumns+` FROM projects p WHERE p.id = $1::uuid`, id).Scan)
	return p, noRows(err)
}

func (s *Store) GetProjectBySlug(ctx context.Context, orgID domain.ID, slug string) (domain.Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx,
		`SELECT `+projectColumns+` FROM projects p
		  WHERE p.organization_id = $1::uuid AND p.slug = $2`, orgID, slug).Scan)
	return p, noRows(err)
}

// ListProjectsFor returns only the projects the principal is a member of. There is no
// "list all projects" query in this package by design.
func (s *Store) ListProjectsFor(ctx context.Context, principalID domain.ID) ([]domain.Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+projectColumns+`
		  FROM projects p
		  JOIN project_memberships m ON m.project_id = p.id
		 WHERE m.principal_id = $1::uuid
		 ORDER BY p.created_at`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		p, err := scanProject(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdateProjectConfig(ctx context.Context, id domain.ID, cfg domain.ProjectConfig) error {
	body, err := marshalJSON(cfg)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	_, err = s.pool.Exec(ctx,
		`UPDATE projects SET config = $2, config_sha = $3 WHERE id = $1::uuid`,
		id, body, hex.EncodeToString(sum[:]))
	return err
}

func (s *Store) SetProjectRepoPath(ctx context.Context, id domain.ID, repoPath string) error {
	_, err := s.pool.Exec(ctx, `UPDATE projects SET repo_path = $2 WHERE id = $1::uuid`, id, repoPath)
	return err
}

// ---------------------------------------------------------------------------
// Workflows
// ---------------------------------------------------------------------------

// PutWorkflow stores a WORKFLOW.md revision and makes it the project's current one.
//
// The content hash is what every attempt records (DESIGN.md §20.4), so a result can always
// be explained by the exact rules that were in force when it ran.
func (s *Store) PutWorkflow(ctx context.Context, projectID domain.ID, content string, frontmatter map[string]any) (string, error) {
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])

	fm, err := marshalJSON(frontmatter)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO workflows (project_id, sha, content, frontmatter)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (project_id, sha) DO UPDATE SET frontmatter = EXCLUDED.frontmatter`,
		projectID, sha, content, fm); err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE projects SET workflow_sha = $2 WHERE id = $1::uuid`, projectID, sha); err != nil {
		return "", err
	}
	return sha, nil
}

type Workflow struct {
	SHA         string         `json:"sha"`
	Content     string         `json:"content"`
	Frontmatter map[string]any `json:"frontmatter"`
}

func (s *Store) GetWorkflow(ctx context.Context, projectID domain.ID, sha string) (Workflow, error) {
	var w Workflow
	var raw []byte
	query := `SELECT sha, content, frontmatter FROM workflows
	           WHERE project_id = $1::uuid AND sha = $2`
	args := []any{projectID, sha}
	if sha == "" {
		// Empty sha means "whatever the project currently points at".
		query = `SELECT w.sha, w.content, w.frontmatter
		           FROM workflows w JOIN projects p ON p.workflow_sha = w.sha AND p.id = w.project_id
		          WHERE p.id = $1::uuid`
		args = []any{projectID}
	}
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&w.SHA, &w.Content, &raw); err != nil {
		return Workflow{}, noRows(err)
	}
	if err := decodeJSON(raw, &w.Frontmatter); err != nil {
		return Workflow{}, err
	}
	return w, nil
}

// ---------------------------------------------------------------------------
// Model profiles
// ---------------------------------------------------------------------------

func (s *Store) UpsertModelProfile(ctx context.Context, p domain.ModelProfile) (domain.ModelProfile, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO model_profiles (organization_id, alias, harness, provider, model,
		            reasoning_effort, capabilities, tier, context_window,
		            input_cost_per_mtok, output_cost_per_mtok, billing, enabled, catalog_version)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (organization_id, alias, harness) DO UPDATE SET
		    provider = EXCLUDED.provider,
		    model = EXCLUDED.model,
		    reasoning_effort = EXCLUDED.reasoning_effort,
		    capabilities = EXCLUDED.capabilities,
		    tier = EXCLUDED.tier,
		    context_window = EXCLUDED.context_window,
		    input_cost_per_mtok = EXCLUDED.input_cost_per_mtok,
		    output_cost_per_mtok = EXCLUDED.output_cost_per_mtok,
		    billing = EXCLUDED.billing,
		    enabled = EXCLUDED.enabled,
		    catalog_version = EXCLUDED.catalog_version
		RETURNING id::text`,
		p.OrganizationID, p.Alias, p.Harness, p.Provider, p.Model,
		p.ReasoningEffort, p.Capabilities, p.Tier, p.ContextWindow,
		p.InputCostPerMTok, p.OutputCostPerMTok, p.Billing, p.Enabled, p.CatalogVersion,
	).Scan(&p.ID)
	return p, err
}

// ListModelProfiles returns the catalog the router resolves aliases against.
func (s *Store) ListModelProfiles(ctx context.Context, orgID domain.ID) ([]domain.ModelProfile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, organization_id::text, alias, harness, provider, model,
		       reasoning_effort, capabilities, tier, context_window,
		       input_cost_per_mtok, output_cost_per_mtok, billing, enabled, catalog_version
		  FROM model_profiles
		 WHERE organization_id = $1::uuid
		 ORDER BY alias, harness`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ModelProfile
	for rows.Next() {
		var p domain.ModelProfile
		if err := rows.Scan(&p.ID, &p.OrganizationID, &p.Alias, &p.Harness, &p.Provider, &p.Model,
			&p.ReasoningEffort, &p.Capabilities, &p.Tier, &p.ContextWindow,
			&p.InputCostPerMTok, &p.OutputCostPerMTok, &p.Billing, &p.Enabled,
			&p.CatalogVersion); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
