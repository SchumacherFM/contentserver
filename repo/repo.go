package repo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/foomo/contentserver/status"

	"github.com/foomo/contentserver/content"
	. "github.com/foomo/contentserver/logger"
	"github.com/foomo/contentserver/requests"
	"github.com/foomo/contentserver/responses"
	"go.uber.org/zap"
)

const maxGetURIForNodeRecursionLevel = 1000

// Dimension dimension in a repo
type Dimension struct {
	Directory    map[string]*content.RepoNode
	URIDirectory map[string]*content.RepoNode
	Node         *content.RepoNode
}

// Repo content repositiory
type Repo struct {
	server    string
	Directory map[string]*Dimension
	// updateLock        sync.Mutex
	dimensionUpdateChannel     chan *repoDimension
	dimensionUpdateDoneChannel chan error

	history                 *history
	updateInProgressChannel chan chan updateResponse

	// jsonBytes []byte
	jsonBuf bytes.Buffer
}

type repoDimension struct {
	Dimension string
	Node      *content.RepoNode
}

// NewRepo constructor
func NewRepo(server string, varDir string) *Repo {

	Log.Info("creating new repo",
		zap.String("server", server),
		zap.String("varDir", varDir),
	)
	repo := &Repo{
		server:                     server,
		Directory:                  map[string]*Dimension{},
		history:                    newHistory(varDir),
		dimensionUpdateChannel:     make(chan *repoDimension),
		dimensionUpdateDoneChannel: make(chan error),
		updateInProgressChannel:    make(chan chan updateResponse, 0),
	}

	go repo.updateRoutine()
	go repo.dimensionUpdateRoutine()

	Log.Info("trying to restore previous state")
	restoreErr := repo.tryToRestoreCurrent()
	if restoreErr != nil {
		Log.Error("	could not restore previous repo content", zap.Error(restoreErr))
	} else {
		Log.Info("restored previous repo content")
	}
	return repo
}

// GetURIs get many uris at once
func (repo *Repo) GetURIs(dimension string, ids []string) map[string]string {
	uris := map[string]string{}
	for _, id := range ids {
		uris[id] = repo.getURI(dimension, id)
	}
	return uris
}

// GetNodes get nodes
func (repo *Repo) GetNodes(r *requests.Nodes) map[string]*content.Node {
	return repo.getNodes(r.Nodes, r.Env)
}

func (repo *Repo) getNodes(nodeRequests map[string]*requests.Node, env *requests.Env) map[string]*content.Node {

	var (
		nodes = map[string]*content.Node{}
		path  = []*content.Item{}
	)
	for nodeName, nodeRequest := range nodeRequests {

		if nodeName == "" || nodeRequest.ID == "" {
			Log.Info("invalid node request", zap.Error(errors.New("nodeName or nodeRequest.ID empty")))
			continue
		}
		Log.Debug("adding node", zap.String("name", nodeName), zap.String("requestID", nodeRequest.ID))

		groups := env.Groups
		if len(nodeRequest.Groups) > 0 {
			groups = nodeRequest.Groups
		}

		dimensionNode, ok := repo.Directory[nodeRequest.Dimension]
		nodes[nodeName] = nil

		if !ok && nodeRequest.Dimension == "" {
			Log.Debug("could not get dimension root node", zap.String("dimension", nodeRequest.Dimension))
			for _, dimension := range env.Dimensions {
				dimensionNode, ok = repo.Directory[dimension]
				if ok {
					Log.Debug("found root node in env.Dimensions", zap.String("dimension", dimension))
					break
				}
				Log.Debug("could NOT find root node in env.Dimensions", zap.String("dimension", dimension))
			}
		}

		if !ok {
			Log.Error("could not get dimension root node", zap.String("nodeRequest.Dimension", nodeRequest.Dimension))
			continue
		}
		treeNode, ok := dimensionNode.Directory[nodeRequest.ID]
		if ok {
			nodes[nodeName] = repo.getNode(treeNode, nodeRequest.Expand, nodeRequest.MimeTypes, path, 0, groups, nodeRequest.DataFields)
		} else {
			Log.Error("an invalid tree node was requested",
				zap.String("nodeName", nodeName),
				zap.String("ID", nodeRequest.ID),
			)
		}
	}
	return nodes
}

// GetContent resolves content and fetches nodes in one call. It combines those
// two tasks for performance reasons.
//
// In the first step it uses r.URI to look up content in all given
// r.Env.Dimensions of repo.Directory.
//
// In the second step it collects the requested nodes.
//
// those two steps are independent.
func (repo *Repo) GetContent(r *requests.Content) (c *content.SiteContent, err error) {
	// add more input validation
	err = repo.validateContentRequest(r)
	if err != nil {
		Log.Error("repo.GetContent invalid request", zap.Error(err))
		return
	}
	Log.Debug("repo.GetContent", zap.String("URI", r.URI))
	c = content.NewSiteContent()
	resolved, resolvedURI, resolvedDimension, node := repo.resolveContent(r.Env.Dimensions, r.URI)
	if resolved {
		if !node.CanBeAccessedByGroups(r.Env.Groups) {
			Log.Warn("resolvecontent got status 401", zap.String("URI", r.URI))
			c.Status = content.StatusForbidden
		} else {
			Log.Info("resolvecontent got status 200", zap.String("URI", r.URI))
			c.Status = content.StatusOk
			c.Data = node.Data
		}
		c.MimeType = node.MimeType
		c.Dimension = resolvedDimension
		c.URI = resolvedURI
		c.Item = node.ToItem(r.DataFields)
		c.Path = node.GetPath()
		// fetch URIs for all dimensions
		uris := make(map[string]string)
		for dimensionName := range repo.Directory {
			uris[dimensionName] = repo.getURI(dimensionName, node.ID)
		}
		c.URIs = uris
	} else {
		Log.Info("resolvecontent got status 404", zap.String("URI", r.URI))
		c.Status = content.StatusNotFound
		c.Dimension = r.Env.Dimensions[0]
	}

	Log.Debug("got content",
		zap.Bool("resolved", resolved),
		zap.String("resolvedURI", resolvedURI),
		zap.String("resolvedDimension", resolvedDimension),
		zap.String("nodeName", node.Name),
	)
	if !resolved {
		Log.Debug("failed to resolve, falling back to default dimension",
			zap.String("URI", r.URI),
			zap.String("defaultDimension", r.Env.Dimensions[0]),
		)
		// r.Env.Dimensions is validated => we can access it
		resolvedDimension = r.Env.Dimensions[0]
	}
	// add navigation trees
	for _, node := range r.Nodes {
		if node.Dimension == "" {
			node.Dimension = resolvedDimension
		}
	}
	c.Nodes = repo.getNodes(r.Nodes, r.Env)
	return c, nil
}

// GetRepo get the whole repo in all dimensions
func (repo *Repo) GetRepo() map[string]*content.RepoNode {
	response := make(map[string]*content.RepoNode)
	for dimensionName, dimension := range repo.Directory {
		response[dimensionName] = dimension.Node
	}
	return response
}

// WriteRepoBytes get the whole repo in all dimensions
// reads the JSON history file from the Filesystem and copies it directly in to the supplied buffer
// the result is wrapped as service response, e.g: {"reply": <contentData>}
func (repo *Repo) WriteRepoBytes(w io.Writer) {

	f, err := os.Open(repo.history.getCurrentFilename())
	if err != nil {
		Log.Error("failed to serve Repo JSON", zap.Error(err))
	}

	w.Write([]byte("{\"reply\":"))
	_, err = io.Copy(w, f)
	if err != nil {
		Log.Error("failed to serve Repo JSON", zap.Error(err))
	}
	w.Write([]byte("}"))
}

// Update - reload contents of repository with json from repo.server
func (repo *Repo) Update() (updateResponse *responses.Update) {
	floatSeconds := func(nanoSeconds int64) float64 {
		return float64(float64(nanoSeconds) / float64(1000000000.0))
	}

	Log.Info("Update triggered")
	// Log.Info(ansi.Yellow + "BUFFER LENGTH BEFORE tryUpdate(): " + strconv.Itoa(len(repo.jsonBuf.Bytes())) + ansi.Reset)

	startTime := time.Now().UnixNano()
	updateRepotime, updateErr := repo.tryUpdate()
	updateResponse = &responses.Update{}
	updateResponse.Stats.RepoRuntime = floatSeconds(updateRepotime)

	if updateErr != nil {
		updateResponse.Success = false
		updateResponse.Stats.NumberOfNodes = -1
		updateResponse.Stats.NumberOfURIs = -1
		// let us try to restore the world from a file
		Log.Error("could not update repository:", zap.Error(updateErr))
		// Log.Info(ansi.Yellow + "BUFFER LENGTH AFTER ERROR: " + strconv.Itoa(len(repo.jsonBuf.Bytes())) + ansi.Reset)
		updateResponse.ErrorMessage = updateErr.Error()

		// only try to restore if the update failed during processing
		if updateErr != errUpdateRejected {
			restoreErr := repo.tryToRestoreCurrent()
			if restoreErr != nil {
				Log.Error("failed to restore preceding repo version", zap.Error(restoreErr))
			} else {
				Log.Info("restored current repo from local history")
			}
		}
	} else {
		updateResponse.Success = true
		// persist the currently loaded one
		historyErr := repo.history.add(repo.jsonBuf.Bytes())
		if historyErr != nil {
			Log.Error("could not persist current repo in history", zap.Error(historyErr))
			status.M.HistoryPersistFailedCounter.WithLabelValues(historyErr.Error()).Inc()
		}
		// add some stats
		for dimension := range repo.Directory {
			updateResponse.Stats.NumberOfNodes += len(repo.Directory[dimension].Directory)
			updateResponse.Stats.NumberOfURIs += len(repo.Directory[dimension].URIDirectory)
		}
	}
	updateResponse.Stats.OwnRuntime = floatSeconds(time.Now().UnixNano()-startTime) - updateResponse.Stats.RepoRuntime
	return updateResponse
}

// resolveContent find content in a repository
func (repo *Repo) resolveContent(dimensions []string, URI string) (resolved bool, resolvedURI string, resolvedDimension string, repoNode *content.RepoNode) {
	parts := strings.Split(URI, content.PathSeparator)
	Log.Debug("repo.ResolveContent", zap.String("URI", URI))
	for i := len(parts); i > 0; i-- {
		testURI := strings.Join(parts[0:i], content.PathSeparator)
		if testURI == "" {
			testURI = content.PathSeparator
		}
		for _, dimension := range dimensions {
			if d, ok := repo.Directory[dimension]; ok {
				Log.Debug("checking",
					zap.String("dimension", dimension),
					zap.String("URI", testURI),
				)
				if repoNode, ok := d.URIDirectory[testURI]; ok {
					resolved = true
					Log.Debug("found node", zap.String("URI", testURI), zap.String("destination", repoNode.DestinationID))
					if len(repoNode.DestinationID) > 0 {
						if destionationNode, destinationNodeOk := d.Directory[repoNode.DestinationID]; destinationNodeOk {
							repoNode = destionationNode
						}
					}
					return true, testURI, dimension, repoNode
				}
			}
		}
	}
	return
}

func (repo *Repo) getURIForNode(dimension string, repoNode *content.RepoNode, recursionLevel int64) (uri string) {
	if len(repoNode.LinkID) == 0 {
		uri = repoNode.URI
		return
	}
	linkedNode, ok := repo.Directory[dimension].Directory[repoNode.LinkID]
	if ok {
		if recursionLevel > maxGetURIForNodeRecursionLevel {
			Log.Error("maxGetURIForNodeRecursionLevel reached", zap.String("repoNode.ID", repoNode.ID), zap.String("linkID", repoNode.LinkID), zap.String("dimension", dimension))
			return ""
		}
		return repo.getURIForNode(dimension, linkedNode, recursionLevel+1)
	}
	return
}

func (repo *Repo) getURI(dimension string, id string) string {
	repoNode, ok := repo.Directory[dimension].Directory[id]
	if ok {
		return repo.getURIForNode(dimension, repoNode, 0)
	}
	return ""
}

func (repo *Repo) getNode(repoNode *content.RepoNode, expanded bool, mimeTypes []string, path []*content.Item, level int, groups []string, dataFields []string) *content.Node {
	node := content.NewNode()
	node.Item = repoNode.ToItem(dataFields)
	Log.Debug("getNode", zap.String("ID", repoNode.ID))
	for _, childID := range repoNode.Index {
		childNode := repoNode.Nodes[childID]
		if (level == 0 || expanded || !expanded && childNode.InPath(path)) && !childNode.Hidden && childNode.CanBeAccessedByGroups(groups) && childNode.IsOneOfTheseMimeTypes(mimeTypes) {
			node.Nodes[childID] = repo.getNode(childNode, expanded, mimeTypes, path, level+1, groups, dataFields)
			node.Index = append(node.Index, childID)
		}
	}
	return node
}

func (repo *Repo) validateContentRequest(req *requests.Content) (err error) {
	if req == nil {
		return errors.New("request must not be nil")
	}
	if len(req.URI) == 0 {
		return errors.New("request URI must not be empty")
	}
	if req.Env == nil {
		return errors.New("request.Env must not be nil")
	}
	if len(req.Env.Dimensions) == 0 {
		return errors.New("request.Env.Dimensions must not be empty")
	}
	for _, envDimension := range req.Env.Dimensions {
		if !repo.hasDimension(envDimension) {
			availableDimensions := []string{}
			for availableDimension := range repo.Directory {
				availableDimensions = append(availableDimensions, availableDimension)
			}
			return errors.New(fmt.Sprint(
				"unknown dimension ", envDimension,
				" in r.Env must be one of ", availableDimensions,
				" repo has ", len(repo.Directory), " dimensions",
			))
		}
	}
	return nil
}

func (repo *Repo) hasDimension(d string) bool {
	_, hasDimension := repo.Directory[d]
	return hasDimension
}

// func uriKeyForState(state string, uri string) string {
// 	return state + "-" + uri
// }
