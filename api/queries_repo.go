package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cli/cli/internal/ghrepo"
)

// Repository contains information about a GitHub repo
type Repository struct {
	ID        string
	Name      string
	URL       string
	CloneURL  string
	CreatedAt time.Time
	Owner     RepositoryOwner

	IsPrivate        bool
	HasIssuesEnabled bool
	ViewerPermission string
	DefaultBranchRef struct {
		Name   string
		Target struct {
			OID string
		}
	}

	Parent *Repository
}

// RepositoryOwner is the owner of a GitHub repository
type RepositoryOwner struct {
	Login string
}

// RepoOwner is the login name of the owner
func (r Repository) RepoOwner() string {
	return r.Owner.Login
}

// RepoName is the name of the repository
func (r Repository) RepoName() string {
	return r.Name
}

// IsFork is true when this repository has a parent repository
func (r Repository) IsFork() bool {
	return r.Parent != nil
}

// ViewerCanPush is true when the requesting user has push access
func (r Repository) ViewerCanPush() bool {
	switch r.ViewerPermission {
	case "ADMIN", "MAINTAIN", "WRITE":
		return true
	default:
		return false
	}
}

func GitHubRepo(client *Client, repo ghrepo.Interface) (*Repository, error) {
	query := `
	query($owner: String!, $name: String!) {
		repository(owner: $owner, name: $name) {
			id
			hasIssuesEnabled
		}
	}`
	variables := map[string]interface{}{
		"owner": repo.RepoOwner(),
		"name":  repo.RepoName(),
	}

	result := struct {
		Repository Repository
	}{}
	err := client.GraphQL(query, variables, &result)

	if err != nil {
		return nil, err
	}

	return &result.Repository, nil
}

// RepoNetworkResult describes the relationship between related repositories
type RepoNetworkResult struct {
	ViewerLogin  string
	Repositories []*Repository
}

// RepoNetwork inspects the relationship between multiple GitHub repositories
func RepoNetwork(client *Client, repos []ghrepo.Interface) (RepoNetworkResult, error) {
	queries := make([]string, 0, len(repos))
	for i, repo := range repos {
		queries = append(queries, fmt.Sprintf(`
		repo_%03d: repository(owner: %q, name: %q) {
			...repo
			parent {
				...repo
			}
		}
		`, i, repo.RepoOwner(), repo.RepoName()))
	}

	// Since the query is constructed dynamically, we can't parse a response
	// format using a static struct. Instead, hold the raw JSON data until we
	// decide how to parse it manually.
	graphqlResult := make(map[string]*json.RawMessage)
	var result RepoNetworkResult

	err := client.GraphQL(fmt.Sprintf(`
	fragment repo on Repository {
		id
		name
		owner { login }
		viewerPermission
		defaultBranchRef {
			name
			target { oid }
		}
		isPrivate
	}
	query {
		viewer { login }
		%s
	}
	`, strings.Join(queries, "")), nil, &graphqlResult)
	graphqlError, isGraphQLError := err.(*GraphQLErrorResponse)
	if isGraphQLError {
		// If the only errors are that certain repositories are not found,
		// continue processing this response instead of returning an error
		tolerated := true
		for _, ge := range graphqlError.Errors {
			if ge.Type != "NOT_FOUND" {
				tolerated = false
			}
		}
		if tolerated {
			err = nil
		}
	}
	if err != nil {
		return result, err
	}

	keys := make([]string, 0, len(graphqlResult))
	for key := range graphqlResult {
		keys = append(keys, key)
	}
	// sort keys to ensure `repo_{N}` entries are processed in order
	sort.Sort(sort.StringSlice(keys))

	// Iterate over keys of GraphQL response data and, based on its name,
	// dynamically allocate the target struct an individual message gets decoded to.
	for _, name := range keys {
		jsonMessage := graphqlResult[name]
		if name == "viewer" {
			viewerResult := struct {
				Login string
			}{}
			decoder := json.NewDecoder(bytes.NewReader([]byte(*jsonMessage)))
			if err := decoder.Decode(&viewerResult); err != nil {
				return result, err
			}
			result.ViewerLogin = viewerResult.Login
		} else if strings.HasPrefix(name, "repo_") {
			if jsonMessage == nil {
				result.Repositories = append(result.Repositories, nil)
				continue
			}
			var repo Repository
			decoder := json.NewDecoder(bytes.NewReader(*jsonMessage))
			if err := decoder.Decode(&repo); err != nil {
				return result, err
			}
			result.Repositories = append(result.Repositories, &repo)
		} else {
			return result, fmt.Errorf("unknown GraphQL result key %q", name)
		}
	}
	return result, nil
}

// repositoryV3 is the repository result from GitHub API v3
type repositoryV3 struct {
	NodeID    string
	Name      string
	CreatedAt time.Time `json:"created_at"`
	CloneURL  string    `json:"clone_url"`
	Owner     struct {
		Login string
	}
}

// ForkRepo forks the repository on GitHub and returns the new repository
func ForkRepo(client *Client, repo ghrepo.Interface) (*Repository, error) {
	path := fmt.Sprintf("repos/%s/forks", ghrepo.FullName(repo))
	body := bytes.NewBufferString(`{}`)
	result := repositoryV3{}
	err := client.REST("POST", path, body, &result)
	if err != nil {
		return nil, err
	}

	return &Repository{
		ID:        result.NodeID,
		Name:      result.Name,
		CloneURL:  result.CloneURL,
		CreatedAt: result.CreatedAt,
		Owner: RepositoryOwner{
			Login: result.Owner.Login,
		},
		ViewerPermission: "WRITE",
	}, nil
}

// RepoCreateInput represents input parameters for RepoCreate
type RepoCreateInput struct {
	Name        string `json:"name"`
	Visibility  string `json:"visibility"`
	Homepage    string `json:"homepage,omitempty"`
	Description string `json:"description,omitempty"`

	OwnerID string `json:"ownerId,omitempty"`
	TeamID  string `json:"teamId,omitempty"`

	HasIssuesEnabled bool `json:"hasIssuesEnabled"`
	HasWikiEnabled   bool `json:"hasWikiEnabled"`
}

// RepoCreate creates a new GitHub repository
func RepoCreate(client *Client, input RepoCreateInput) (*Repository, error) {
	var response struct {
		CreateRepository struct {
			Repository Repository
		}
	}

	if input.TeamID != "" {
		orgID, teamID, err := resolveOrganizationTeam(client, input.OwnerID, input.TeamID)
		if err != nil {
			return nil, err
		}
		input.TeamID = teamID
		input.OwnerID = orgID
	} else if input.OwnerID != "" {
		orgID, err := resolveOrganization(client, input.OwnerID)
		if err != nil {
			return nil, err
		}
		input.OwnerID = orgID
	}

	variables := map[string]interface{}{
		"input": input,
	}

	err := client.GraphQL(`
	mutation($input: CreateRepositoryInput!) {
		createRepository(input: $input) {
			repository {
				id
				name
				owner { login }
				url
			}
		}
	}
	`, variables, &response)
	if err != nil {
		return nil, err
	}

	return &response.CreateRepository.Repository, nil
}
