package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/shurcooL/githubv4"
	"golang.org/x/sync/errgroup"

	"github.com/MichaelMure/git-bug/bridge/core"
	"github.com/MichaelMure/git-bug/bug"
	"github.com/MichaelMure/git-bug/cache"
	"github.com/MichaelMure/git-bug/entity"
)

var (
	ErrMissingIdentityToken = errors.New("missing identity token")
)

// githubExporter implement the Exporter interface
type githubExporter struct {
	conf core.Configuration

	// cache identities clients
	identityClient map[entity.Id]*githubv4.Client

	// map identities with their tokens
	identityToken map[entity.Id]string

	// github repository ID
	repositoryID string

	// cache identifiers used to speed up exporting operations
	// cleared for each bug
	cachedOperationIDs map[entity.Id]string

	// cache labels used to speed up exporting labels events
	cachedLabels map[string]string
}

// Init .
func (ge *githubExporter) Init(conf core.Configuration) error {
	ge.conf = conf
	//TODO: initialize with multiple tokens
	ge.identityToken = make(map[entity.Id]string)
	ge.identityClient = make(map[entity.Id]*githubv4.Client)
	ge.cachedOperationIDs = make(map[entity.Id]string)
	ge.cachedLabels = make(map[string]string)
	return nil
}

// getIdentityClient return a githubv4 API client configured with the access token of the given identity.
// if no client were found it will initialize it from the known tokens map and cache it for next use
func (ge *githubExporter) getIdentityClient(id entity.Id) (*githubv4.Client, error) {
	client, ok := ge.identityClient[id]
	if ok {
		return client, nil
	}

	// get token
	token, ok := ge.identityToken[id]
	if !ok {
		return nil, ErrMissingIdentityToken
	}

	// create client
	client = buildClient(token)
	// cache client
	ge.identityClient[id] = client

	return client, nil
}

// ExportAll export all event made by the current user to Github
func (ge *githubExporter) ExportAll(ctx context.Context, repo *cache.RepoCache, since time.Time) (<-chan core.ExportResult, error) {
	out := make(chan core.ExportResult)

	user, err := repo.GetUserIdentity()
	if err != nil {
		return nil, err
	}

	ge.identityToken[user.Id()] = ge.conf[core.ConfigKeyToken]

	// get repository node id
	ge.repositoryID, err = getRepositoryNodeID(
		ctx,
		ge.conf[keyOwner],
		ge.conf[keyProject],
		ge.conf[core.ConfigKeyToken],
	)

	if err != nil {
		return nil, err
	}

	client, err := ge.getIdentityClient(user.Id())
	if err != nil {
		return nil, err
	}

	// query all labels
	err = ge.cacheGithubLabels(ctx, client)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(out)

		var allIdentitiesIds []entity.Id
		for id := range ge.identityToken {
			allIdentitiesIds = append(allIdentitiesIds, id)
		}

		allBugsIds := repo.AllBugsIds()

		for _, id := range allBugsIds {
			b, err := repo.ResolveBug(id)
			if err != nil {
				out <- core.NewExportError(errors.Wrap(err, "can't load bug"), id)
				return
			}

			select {

			case <-ctx.Done():
				// stop iterating if context cancel function is called
				return

			default:
				snapshot := b.Snapshot()

				// ignore issues created before since date
				// TODO: compare the Lamport time instead of using the unix time
				if snapshot.CreatedAt.Before(since) {
					out <- core.NewExportNothing(b.Id(), "bug created before the since date")
					continue
				}

				if snapshot.HasAnyActor(allIdentitiesIds...) {
					// try to export the bug and it associated events
					ge.exportBug(ctx, b, since, out)
				}
			}
		}
	}()

	return out, nil
}

// exportBug publish bugs and related events
func (ge *githubExporter) exportBug(ctx context.Context, b *cache.BugCache, since time.Time, out chan<- core.ExportResult) {
	snapshot := b.Snapshot()
	var bugUpdated bool

	var bugGithubID string
	var bugGithubURL string

	// Special case:
	// if a user try to export a bug that is not already exported to Github (or imported
	// from Github) and we do not have the token of the bug author, there is nothing we can do.

	// first operation is always createOp
	createOp := snapshot.Operations[0].(*bug.CreateOperation)
	author := snapshot.Author

	// skip bug if origin is not allowed
	origin, ok := snapshot.GetCreateMetadata(core.MetaKeyOrigin)
	if ok && origin != target {
		out <- core.NewExportNothing(b.Id(), fmt.Sprintf("issue tagged with origin: %s", origin))
		return
	}

	// get github bug ID
	githubID, ok := snapshot.GetCreateMetadata(metaKeyGithubId)
	if ok {
		githubURL, ok := snapshot.GetCreateMetadata(metaKeyGithubUrl)
		if !ok {
			// if we find github ID, github URL must be found too
			err := fmt.Errorf("incomplete Github metadata: expected to find issue URL")
			out <- core.NewExportError(err, b.Id())
		}

		// extract owner and project
		owner, project, err := splitURL(githubURL)
		if err != nil {
			err := fmt.Errorf("bad project url: %v", err)
			out <- core.NewExportError(err, b.Id())
			return
		}

		// ignore issue coming from other repositories
		if owner != ge.conf[keyOwner] && project != ge.conf[keyProject] {
			out <- core.NewExportNothing(b.Id(), fmt.Sprintf("skipping issue from url:%s", githubURL))
			return
		}

		// will be used to mark operation related to a bug as exported
		bugGithubID = githubID
		bugGithubURL = githubURL

	} else {
		// check that we have a token for operation author
		client, err := ge.getIdentityClient(author.Id())
		if err != nil {
			// if bug is still not exported and we do not have the author stop the execution
			out <- core.NewExportNothing(b.Id(), fmt.Sprintf("missing author token"))
			return
		}

		// create bug
		id, url, err := createGithubIssue(ctx, client, ge.repositoryID, createOp.Title, createOp.Message)
		if err != nil {
			err := errors.Wrap(err, "exporting github issue")
			out <- core.NewExportError(err, b.Id())
			return
		}

		out <- core.NewExportBug(b.Id())

		// mark bug creation operation as exported
		if err := markOperationAsExported(b, createOp.Id(), id, url); err != nil {
			err := errors.Wrap(err, "marking operation as exported")
			out <- core.NewExportError(err, b.Id())
			return
		}

		// commit operation to avoid creating multiple issues with multiple pushes
		if err := b.CommitAsNeeded(); err != nil {
			err := errors.Wrap(err, "bug commit")
			out <- core.NewExportError(err, b.Id())
			return
		}

		// cache bug github ID and URL
		bugGithubID = id
		bugGithubURL = url
	}

	// cache operation github id
	ge.cachedOperationIDs[createOp.Id()] = bugGithubID

	for _, op := range snapshot.Operations[1:] {
		// ignore SetMetadata operations
		if _, ok := op.(*bug.SetMetadataOperation); ok {
			continue
		}

		// ignore operations already existing in github (due to import or export)
		// cache the ID of already exported or imported issues and events from Github
		if id, ok := op.GetMetadata(metaKeyGithubId); ok {
			ge.cachedOperationIDs[op.Id()] = id
			continue
		}

		opAuthor := op.GetAuthor()
		client, err := ge.getIdentityClient(opAuthor.Id())
		if err != nil {
			continue
		}

		var id, url string
		switch op := op.(type) {
		case *bug.AddCommentOperation:
			// send operation to github
			id, url, err = addCommentGithubIssue(ctx, client, bugGithubID, op.Message)
			if err != nil {
				err := errors.Wrap(err, "adding comment")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportComment(op.Id())

			// cache comment id
			ge.cachedOperationIDs[op.Id()] = id

		case *bug.EditCommentOperation:
			// Since github doesn't consider the issue body as a comment
			if op.Target == createOp.Id() {

				// case bug creation operation: we need to edit the Github issue
				if err := updateGithubIssueBody(ctx, client, bugGithubID, op.Message); err != nil {
					err := errors.Wrap(err, "editing issue")
					out <- core.NewExportError(err, b.Id())
					return
				}

				out <- core.NewExportCommentEdition(op.Id())

				id = bugGithubID
				url = bugGithubURL

			} else {

				// case comment edition operation: we need to edit the Github comment
				commentID, ok := ge.cachedOperationIDs[op.Target]
				if !ok {
					panic("unexpected error: comment id not found")
				}

				eid, eurl, err := editCommentGithubIssue(ctx, client, commentID, op.Message)
				if err != nil {
					err := errors.Wrap(err, "editing comment")
					out <- core.NewExportError(err, b.Id())
					return
				}

				out <- core.NewExportCommentEdition(op.Id())

				// use comment id/url instead of issue id/url
				id = eid
				url = eurl
			}

		case *bug.SetStatusOperation:
			if err := updateGithubIssueStatus(ctx, client, bugGithubID, op.Status); err != nil {
				err := errors.Wrap(err, "editing status")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportStatusChange(op.Id())

			id = bugGithubID
			url = bugGithubURL

		case *bug.SetTitleOperation:
			if err := updateGithubIssueTitle(ctx, client, bugGithubID, op.Title); err != nil {
				err := errors.Wrap(err, "editing title")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportTitleEdition(op.Id())

			id = bugGithubID
			url = bugGithubURL

		case *bug.LabelChangeOperation:
			if err := ge.updateGithubIssueLabels(ctx, client, bugGithubID, op.Added, op.Removed); err != nil {
				err := errors.Wrap(err, "updating labels")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportLabelChange(op.Id())

			id = bugGithubID
			url = bugGithubURL

		default:
			panic("unhandled operation type case")
		}

		// mark operation as exported
		if err := markOperationAsExported(b, op.Id(), id, url); err != nil {
			err := errors.Wrap(err, "marking operation as exported")
			out <- core.NewExportError(err, b.Id())
			return
		}

		// commit at each operation export to avoid exporting same events multiple times
		if err := b.CommitAsNeeded(); err != nil {
			err := errors.Wrap(err, "bug commit")
			out <- core.NewExportError(err, b.Id())
			return
		}

		bugUpdated = true
	}

	if !bugUpdated {
		out <- core.NewExportNothing(b.Id(), "nothing has been exported")
	}
}

// getRepositoryNodeID request github api v3 to get repository node id
func getRepositoryNodeID(ctx context.Context, owner, project, token string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", githubV3Url, owner, project)
	client := &http.Client{}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	// need the token for private repositories
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error %v retrieving repository node id", resp.StatusCode)
	}

	aux := struct {
		NodeID string `json:"node_id"`
	}{}

	data, _ := ioutil.ReadAll(resp.Body)
	err = resp.Body.Close()
	if err != nil {
		return "", err
	}

	err = json.Unmarshal(data, &aux)
	if err != nil {
		return "", err
	}

	return aux.NodeID, nil
}

func markOperationAsExported(b *cache.BugCache, target entity.Id, githubID, githubURL string) error {
	_, err := b.SetMetadata(
		target,
		map[string]string{
			metaKeyGithubId:  githubID,
			metaKeyGithubUrl: githubURL,
		},
	)

	return err
}

func (ge *githubExporter) cacheGithubLabels(ctx context.Context, gc *githubv4.Client) error {
	variables := map[string]interface{}{
		"owner": githubv4.String(ge.conf[keyOwner]),
		"name":  githubv4.String(ge.conf[keyProject]),
		"first": githubv4.Int(10),
		"after": (*githubv4.String)(nil),
	}

	q := labelsQuery{}

	hasNextPage := true
	for hasNextPage {
		// create a new timeout context at each iteration
		ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
		defer cancel()

		if err := gc.Query(ctx, &q, variables); err != nil {
			return err
		}

		for _, label := range q.Repository.Labels.Nodes {
			ge.cachedLabels[label.Name] = label.ID
		}

		hasNextPage = q.Repository.Labels.PageInfo.HasNextPage
		variables["after"] = q.Repository.Labels.PageInfo.EndCursor
	}

	return nil
}

func (ge *githubExporter) getLabelID(gc *githubv4.Client, label string) (string, error) {
	label = strings.ToLower(label)
	for cachedLabel, ID := range ge.cachedLabels {
		if label == strings.ToLower(cachedLabel) {
			return ID, nil
		}
	}

	return "", fmt.Errorf("didn't find label id in cache")
}

// create a new label and return it github id
// NOTE: since createLabel mutation is still in preview mode we use github api v3 to create labels
// see https://developer.github.com/v4/mutation/createlabel/ and https://developer.github.com/v4/previews/#labels-preview
func (ge *githubExporter) createGithubLabel(ctx context.Context, label, color string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/labels", githubV3Url, ge.conf[keyOwner], ge.conf[keyProject])
	client := &http.Client{}

	params := struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}{
		Name:  label,
		Color: color,
	}

	data, err := json.Marshal(params)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	// need the token for private repositories
	req.Header.Set("Authorization", fmt.Sprintf("token %s", ge.conf[core.ConfigKeyToken]))

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("error creating label: response status %v", resp.StatusCode)
	}

	aux := struct {
		ID     int    `json:"id"`
		NodeID string `json:"node_id"`
		Color  string `json:"color"`
	}{}

	data, _ = ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()

	err = json.Unmarshal(data, &aux)
	if err != nil {
		return "", err
	}

	return aux.NodeID, nil
}

/**
// create github label using api v4
func (ge *githubExporter) createGithubLabelV4(gc *githubv4.Client, label, labelColor string) (string, error) {
	m := createLabelMutation{}
	input := createLabelInput{
		RepositoryID: ge.repositoryID,
		Name:         githubv4.String(label),
		Color:        githubv4.String(labelColor),
	}

	parentCtx := context.Background()
	ctx, cancel := context.WithTimeout(parentCtx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, &m, input, nil); err != nil {
		return "", err
	}

	return m.CreateLabel.Label.ID, nil
}
*/

func (ge *githubExporter) getOrCreateGithubLabelID(ctx context.Context, gc *githubv4.Client, repositoryID string, label bug.Label) (string, error) {
	// try to get label id from cache
	labelID, err := ge.getLabelID(gc, string(label))
	if err == nil {
		return labelID, nil
	}

	// RGBA to hex color
	rgba := label.Color().RGBA()
	hexColor := fmt.Sprintf("%.2x%.2x%.2x", rgba.R, rgba.G, rgba.B)

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	labelID, err = ge.createGithubLabel(ctx, string(label), hexColor)
	if err != nil {
		return "", err
	}

	return labelID, nil
}

func (ge *githubExporter) getLabelsIDs(ctx context.Context, gc *githubv4.Client, repositoryID string, labels []bug.Label) ([]githubv4.ID, error) {
	ids := make([]githubv4.ID, 0, len(labels))
	var err error

	// check labels ids
	for _, label := range labels {
		id, ok := ge.cachedLabels[string(label)]
		if !ok {
			// try to query label id
			id, err = ge.getOrCreateGithubLabelID(ctx, gc, repositoryID, label)
			if err != nil {
				return nil, errors.Wrap(err, "get or create github label")
			}

			// cache label id
			ge.cachedLabels[string(label)] = id
		}

		ids = append(ids, githubv4.ID(id))
	}

	return ids, nil
}

// create a github issue and return it ID
func createGithubIssue(ctx context.Context, gc *githubv4.Client, repositoryID, title, body string) (string, string, error) {
	m := &createIssueMutation{}
	input := githubv4.CreateIssueInput{
		RepositoryID: repositoryID,
		Title:        githubv4.String(title),
		Body:         (*githubv4.String)(&body),
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, m, input, nil); err != nil {
		return "", "", err
	}

	issue := m.CreateIssue.Issue
	return issue.ID, issue.URL, nil
}

// add a comment to an issue and return it ID
func addCommentGithubIssue(ctx context.Context, gc *githubv4.Client, subjectID string, body string) (string, string, error) {
	m := &addCommentToIssueMutation{}
	input := githubv4.AddCommentInput{
		SubjectID: subjectID,
		Body:      githubv4.String(body),
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, m, input, nil); err != nil {
		return "", "", err
	}

	node := m.AddComment.CommentEdge.Node
	return node.ID, node.URL, nil
}

func editCommentGithubIssue(ctx context.Context, gc *githubv4.Client, commentID, body string) (string, string, error) {
	m := &updateIssueCommentMutation{}
	input := githubv4.UpdateIssueCommentInput{
		ID:   commentID,
		Body: githubv4.String(body),
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, m, input, nil); err != nil {
		return "", "", err
	}

	return commentID, m.UpdateIssueComment.IssueComment.URL, nil
}

func updateGithubIssueStatus(ctx context.Context, gc *githubv4.Client, id string, status bug.Status) error {
	m := &updateIssueMutation{}

	// set state
	var state githubv4.IssueState

	switch status {
	case bug.OpenStatus:
		state = githubv4.IssueStateOpen
	case bug.ClosedStatus:
		state = githubv4.IssueStateClosed
	default:
		panic("unknown bug state")
	}

	input := githubv4.UpdateIssueInput{
		ID:    id,
		State: &state,
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, m, input, nil); err != nil {
		return err
	}

	return nil
}

func updateGithubIssueBody(ctx context.Context, gc *githubv4.Client, id string, body string) error {
	m := &updateIssueMutation{}
	input := githubv4.UpdateIssueInput{
		ID:   id,
		Body: (*githubv4.String)(&body),
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, m, input, nil); err != nil {
		return err
	}

	return nil
}

func updateGithubIssueTitle(ctx context.Context, gc *githubv4.Client, id, title string) error {
	m := &updateIssueMutation{}
	input := githubv4.UpdateIssueInput{
		ID:    id,
		Title: (*githubv4.String)(&title),
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	if err := gc.Mutate(ctx, m, input, nil); err != nil {
		return err
	}

	return nil
}

// update github issue labels
func (ge *githubExporter) updateGithubIssueLabels(ctx context.Context, gc *githubv4.Client, labelableID string, added, removed []bug.Label) error {
	reqCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	wg, ctx := errgroup.WithContext(ctx)
	if len(added) > 0 {
		wg.Go(func() error {
			addedIDs, err := ge.getLabelsIDs(ctx, gc, labelableID, added)
			if err != nil {
				return err
			}

			m := &addLabelsToLabelableMutation{}
			inputAdd := githubv4.AddLabelsToLabelableInput{
				LabelableID: labelableID,
				LabelIDs:    addedIDs,
			}

			// add labels
			if err := gc.Mutate(reqCtx, m, inputAdd, nil); err != nil {
				return err
			}
			return nil
		})
	}

	if len(removed) > 0 {
		wg.Go(func() error {
			removedIDs, err := ge.getLabelsIDs(ctx, gc, labelableID, removed)
			if err != nil {
				return err
			}

			m2 := &removeLabelsFromLabelableMutation{}
			inputRemove := githubv4.RemoveLabelsFromLabelableInput{
				LabelableID: labelableID,
				LabelIDs:    removedIDs,
			}

			// remove label labels
			if err := gc.Mutate(reqCtx, m2, inputRemove, nil); err != nil {
				return err
			}
			return nil
		})
	}

	return wg.Wait()
}
