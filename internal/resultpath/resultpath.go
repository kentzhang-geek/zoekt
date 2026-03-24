package resultpath

import (
	"context"

	"github.com/grafana/regexp"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

// RepoMetadataByName returns repository metadata for the repositories present
// in files. Files without a repository name are ignored.
func RepoMetadataByName(ctx context.Context, searcher zoekt.Searcher, files []zoekt.FileMatch) (map[string]*zoekt.Repository, error) {
	repoNames := map[string]struct{}{}
	for _, file := range files {
		if file.Repository == "" {
			continue
		}
		repoNames[file.Repository] = struct{}{}
	}

	if len(repoNames) == 0 {
		return nil, nil
	}

	qs := make([]query.Q, 0, len(repoNames))
	for repoName := range repoNames {
		repoRe, err := regexp.Compile("^" + regexp.QuoteMeta(repoName) + "$")
		if err != nil {
			return nil, err
		}
		qs = append(qs, &query.Repo{Regexp: repoRe})
	}

	repoList, err := searcher.List(ctx, query.NewOr(qs...), &zoekt.ListOptions{Field: zoekt.RepoListFieldRepos})
	if err != nil {
		return nil, err
	}

	repos := make(map[string]*zoekt.Repository, len(repoList.Repos))
	for _, entry := range repoList.Repos {
		repo := entry.Repository
		repos[repo.Name] = &repo
	}

	return repos, nil
}
