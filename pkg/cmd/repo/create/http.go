package create

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghinstance"
	"github.com/cli/cli/v2/internal/ghrepo"
)

// repoCreateInput is input parameters for the repoCreate method
type repoCreateInput struct {
	Name                 string
	HomepageURL          string
	Description          string
	Visibility           string
	OwnerLogin           string
	TeamSlug             string
	TemplateRepositoryID string
	HasIssuesEnabled     bool
	HasWikiEnabled       bool
	GitIgnoreTemplate    string
	LicenseTemplate      string
}

// createRepositoryInputV3 is the payload for the repo create REST API
type createRepositoryInputV3 struct {
	Name              string `json:"name"`
	HomepageURL       string `json:"homepage,omitempty"`
	Description       string `json:"description,omitempty"`
	IsPrivate         bool   `json:"private"`
	Visibility        string `json:"visibility,omitempty"`
	TeamID            uint64 `json:"team_id,omitempty"`
	HasIssuesEnabled  bool   `json:"has_issues"`
	HasWikiEnabled    bool   `json:"has_wiki"`
	GitIgnoreTemplate string `json:"gitignore_template,omitempty"`
	LicenseTemplate   string `json:"license_template,omitempty"`
}

// createRepositoryInput is the payload for the repo create GraphQL mutation
type createRepositoryInput struct {
	Name             string `json:"name"`
	HomepageURL      string `json:"homepageUrl,omitempty"`
	Description      string `json:"description,omitempty"`
	Visibility       string `json:"visibility"`
	OwnerID          string `json:"ownerId,omitempty"`
	TeamID           string `json:"teamId,omitempty"`
	HasIssuesEnabled bool   `json:"hasIssuesEnabled"`
	HasWikiEnabled   bool   `json:"hasWikiEnabled"`
}

// cloneTemplateRepositoryInput is the payload for creating a repo from a template using GraphQL
type cloneTemplateRepositoryInput struct {
	Name         string `json:"name"`
	Visibility   string `json:"visibility"`
	Description  string `json:"description,omitempty"`
	OwnerID      string `json:"ownerId"`
	RepositoryID string `json:"repositoryId"`
}

type createRepositoryFromStackInput struct {
	Name                string                 `json:"name"`
	Private             bool                   `json:"private"`
	Description         string                 `json:"description,omitempty"`
	OwnerLogin          string                 `json:"ownerLogin"`
	StackOwnerLogin     string                 `json:"stackOwnerLogin"`
	StackRepositoryName string                 `json:"stackRepositoryName"`
	ReleaseTag          string                 `json:"releaseTag"`
	Inputs              map[string]interface{} `json:"inputs"`
}

type stackData struct {
	Inputs []map[string]interface{}
	Apps   []map[string]interface{}
}

// repoCreate creates a new GitHub repository
func repoCreate(client *http.Client, hostname string, input repoCreateInput) (*api.Repository, error) {
	isOrg := false
	var ownerID string
	var teamID string
	var teamIDv3 uint64

	apiClient := api.NewClientFromHTTP(client)

	if input.TeamSlug != "" {
		team, err := resolveOrganizationTeam(apiClient, hostname, input.OwnerLogin, input.TeamSlug)
		if err != nil {
			return nil, err
		}
		teamIDv3 = team.ID
		teamID = team.NodeID
		ownerID = team.Organization.NodeID
		isOrg = true
	} else if input.OwnerLogin != "" {
		owner, err := resolveOwner(apiClient, hostname, input.OwnerLogin)
		if err != nil {
			return nil, err
		}
		ownerID = owner.NodeID
		isOrg = owner.IsOrganization()
	}

	if input.TemplateRepositoryID != "" {
		var response struct {
			CloneTemplateRepository struct {
				Repository api.Repository
			}
		}

		if ownerID == "" {
			var err error
			ownerID, err = api.CurrentUserID(apiClient, hostname)
			if err != nil {
				return nil, err
			}
		}

		variables := map[string]interface{}{
			"input": cloneTemplateRepositoryInput{
				Name:         input.Name,
				Description:  input.Description,
				Visibility:   strings.ToUpper(input.Visibility),
				OwnerID:      ownerID,
				RepositoryID: input.TemplateRepositoryID,
			},
		}

		err := apiClient.GraphQL(hostname, `
		mutation CloneTemplateRepository($input: CloneTemplateRepositoryInput!) {
			cloneTemplateRepository(input: $input) {
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

		return api.InitRepoHostname(&response.CloneTemplateRepository.Repository, hostname), nil
	}

	if input.GitIgnoreTemplate != "" || input.LicenseTemplate != "" {
		inputv3 := createRepositoryInputV3{
			Name:              input.Name,
			HomepageURL:       input.HomepageURL,
			Description:       input.Description,
			IsPrivate:         strings.EqualFold(input.Visibility, "PRIVATE"),
			TeamID:            teamIDv3,
			HasIssuesEnabled:  input.HasIssuesEnabled,
			HasWikiEnabled:    input.HasWikiEnabled,
			GitIgnoreTemplate: input.GitIgnoreTemplate,
			LicenseTemplate:   input.LicenseTemplate,
		}

		path := "user/repos"
		if isOrg {
			path = fmt.Sprintf("orgs/%s/repos", input.OwnerLogin)
			inputv3.Visibility = strings.ToLower(input.Visibility)
		}

		body := &bytes.Buffer{}
		enc := json.NewEncoder(body)
		if err := enc.Encode(inputv3); err != nil {
			return nil, err
		}

		repo, err := api.CreateRepoTransformToV4(apiClient, hostname, "POST", path, body)
		if err != nil {
			return nil, err
		}
		return repo, nil
	}

	var response struct {
		CreateRepository struct {
			Repository api.Repository
		}
	}

	variables := map[string]interface{}{
		"input": createRepositoryInput{
			Name:             input.Name,
			Description:      input.Description,
			HomepageURL:      input.HomepageURL,
			Visibility:       strings.ToUpper(input.Visibility),
			OwnerID:          ownerID,
			TeamID:           teamID,
			HasIssuesEnabled: input.HasIssuesEnabled,
			HasWikiEnabled:   input.HasWikiEnabled,
		},
	}

	err := apiClient.GraphQL(hostname, `
	mutation RepositoryCreate($input: CreateRepositoryInput!) {
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

	return api.InitRepoHostname(&response.CreateRepository.Repository, hostname), nil
}

func listReleases(opts *CreateOptions, httpClient *http.Client, baseRepo ghrepo.Interface, hostname string) ([]string, error) {
	url := fmt.Sprintf("%srepos/%s/%s/releases", ghinstance.RESTPrefix(hostname), baseRepo.RepoOwner(), baseRepo.RepoName())
	// stdout := opts.IO.Out
	// fmt.Fprintf(stdout, "GET releases call url: %s\n", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	var releasesResponse []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&releasesResponse)
	releases := []string{}
	for _, release := range releasesResponse {
		// fmt.Fprintf(stdout, "Release tag: %s\n", release["tag_name"].(string))
		releases = append(releases, release["tag_name"].(string))
	}

	return releases, nil
}

func getStackData(opts *CreateOptions, httpClient *http.Client, releaseTag string, baseRepo ghrepo.Interface, hostname string) (stackData, error) {
	url := fmt.Sprintf("%srepos/%s/%s/stacks/data?release_tag=%s", ghinstance.RESTPrefix(hostname), baseRepo.RepoOwner(), baseRepo.RepoName(), releaseTag)
	// stdout := opts.IO.Out
	// fmt.Fprintf(stdout, "GET call url: %s\n", url)
	var data stackData

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return data, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return data, err
	}

	// var stackMetadata map[string][]map[string]string
	var stackMetadata map[string][]map[string]interface{}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&stackMetadata)
	// fmt.Println("---\n---\n", stackMetadata)
	data.Inputs = stackMetadata["inputs"]
	data.Apps = stackMetadata["apps"]

	return data, nil
}

func createRepoFromStack(opts *CreateOptions, httpClient *http.Client, baseRepo ghrepo.Interface, hostname string, input createRepositoryFromStackInput) (io.ReadCloser, error) {
	url := fmt.Sprintf("%srepos/%s/%s/stacks", ghinstance.RESTPrefix(hostname), baseRepo.RepoOwner(), baseRepo.RepoName())
	// stdout := opts.IO.Out
	// fmt.Fprintf(stdout, "POST call url: %s\n", url)
	payload := map[string]interface{}{
		"owner":       input.OwnerLogin,
		"name":        input.Name,
		"release_tag": input.ReleaseTag,
		"inputs":      input.Inputs,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

type ownerResponse struct {
	NodeID string `json:"node_id"`
	Type   string `json:"type"`
}

func (r *ownerResponse) IsOrganization() bool {
	return r.Type == "Organization"
}

func resolveOwner(client *api.Client, hostname, orgName string) (*ownerResponse, error) {
	var response ownerResponse
	err := client.REST(hostname, "GET", fmt.Sprintf("users/%s", orgName), nil, &response)
	return &response, err
}

type teamResponse struct {
	ID           uint64 `json:"id"`
	NodeID       string `json:"node_id"`
	Organization struct {
		NodeID string `json:"node_id"`
	}
}

func resolveOrganizationTeam(client *api.Client, hostname, orgName, teamSlug string) (*teamResponse, error) {
	var response teamResponse
	err := client.REST(hostname, "GET", fmt.Sprintf("orgs/%s/teams/%s", orgName, teamSlug), nil, &response)
	return &response, err
}

// listGitIgnoreTemplates uses API v3 here because gitignore template isn't supported by GraphQL yet.
func listGitIgnoreTemplates(httpClient *http.Client, hostname string) ([]string, error) {
	var gitIgnoreTemplates []string
	client := api.NewClientFromHTTP(httpClient)
	err := client.REST(hostname, "GET", "gitignore/templates", nil, &gitIgnoreTemplates)
	if err != nil {
		return []string{}, err
	}
	return gitIgnoreTemplates, nil
}

// listLicenseTemplates uses API v3 here because license template isn't supported by GraphQL yet.
func listLicenseTemplates(httpClient *http.Client, hostname string) ([]api.License, error) {
	var licenseTemplates []api.License
	client := api.NewClientFromHTTP(httpClient)
	err := client.REST(hostname, "GET", "licenses", nil, &licenseTemplates)
	if err != nil {
		return nil, err
	}
	return licenseTemplates, nil
}
