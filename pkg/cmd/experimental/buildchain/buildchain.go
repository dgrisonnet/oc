package buildchain

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	cmdutil "github.com/GoogleCloudPlatform/kubernetes/pkg/kubectl/cmd/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	dot "github.com/awalterschulze/gographviz"
	"github.com/golang/glog"
	"github.com/spf13/cobra"

	buildapi "github.com/openshift/origin/pkg/build/api"
	buildutil "github.com/openshift/origin/pkg/build/util"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"
	imageapi "github.com/openshift/origin/pkg/image/api"
)

const longDescription = `Output build dependencies of a specific image repository.
Supported output formats are json, dot, and ast. The default is set to json.
Tag and namespace are optional and if they are not specified, 'latest' and the 
default namespace will be used respectively.

Examples:

    # Build dependency tree for the specified image repository and tag
    $ openshift ex build-chain [image-repository]:[tag]

    # Build dependency trees for all tags in the specified image repository
    $ openshift ex build-chain [image-repository] --all-tags

    # Build the dependency tree using tag 'latest' in 'testing' namespace
    $ openshift ex build-chain [image-repository] -n testing

    # Build the dependency tree and output it in DOT syntax
    $ openshift ex build-chain [image-repository] -o dot

    # Build dependency trees for all image repositories in the current namespace
    $ openshift ex build-chain

    # Build dependency trees for all image repositories across all namespaces
    $ openshift ex build-chain --all
`

// ImageRepo is a representation of a node inside a tree
type ImageRepo struct {
	FullName string       `json:"fullname"`
	Tags     []string     `json:"tags,omitempty"`
	Edges    []*Edge      `json:"edges,omitempty"`
	Children []*ImageRepo `json:"children,omitempty"`
}

// String helps in dumping a tree in AST format
func (root *ImageRepo) String() string {
	tree := ""
	tree += root.FullName

	for _, n := range root.Children {
		tree += "(" + n.String() + ")"
	}

	return tree
}

// Edge represents a build configuration relationship
// between two nodes
//
// Note that this type has no relation with the dot.Edge
// type
type Edge struct {
	FullName string `json:"fullname"`
	To       string `json:"to"`
}

// NewEdge adds a new edge on a parent node
//
// Note that this function has no relation
// with the dot.Edge type
func NewEdge(fullname, to string) *Edge {
	return &Edge{
		FullName: fullname,
		To:       to,
	}
}

// NewCmdBuildChain implements the OpenShift experimental build-chain command
func NewCmdBuildChain(f *clientcmd.Factory, parentName, name string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("%s [image-repository]:[tag]", name),
		Short: "Output build dependencies of a specific image repository",
		Long:  longDescription,
		Run: func(cmd *cobra.Command, args []string) {
			err := RunBuildChain(f, cmd, args)
			cmdutil.CheckErr(err)
		},
	}

	cmd.Flags().Bool("all", false, "Build dependency trees for all image repositories")
	cmd.Flags().Bool("all-tags", false, "Build dependency trees for all tags of a specific image repository")
	cmd.Flags().StringP("output", "o", "json", "Output format of dependency tree(s)")
	return cmd
}

// RunBuildChain contains all the necessary functionality for the OpenShift
// experimental build-chain command
func RunBuildChain(f *clientcmd.Factory, cmd *cobra.Command, args []string) error {
	all := cmdutil.GetFlagBool(cmd, "all")
	allTags := cmdutil.GetFlagBool(cmd, "all-tags")
	if len(args) > 1 ||
		(len(args) == 1 && all) ||
		(len(args) == 0 && allTags) ||
		(all && allTags) {
		return cmdutil.UsageError(cmd, "Must pass nothing, an image repository name:tag combination, or specify the --all flag")
	}

	osc, _, err := f.Clients()
	if err != nil {
		return err
	}

	// Retrieve namespace(s)
	namespace := cmdutil.GetFlagString(cmd, "namespace")
	if len(namespace) == 0 {
		namespace, err = f.DefaultNamespace()
		if err != nil {
			return err
		}
	}
	namespaces := make([]string, 0)
	if all {
		projectList, err := osc.Projects().List(labels.Everything(), fields.Everything())
		if err != nil {
			return err
		}
		for _, project := range projectList.Items {
			glog.V(4).Infof("Found namespace %s", project.Name)
			namespaces = append(namespaces, project.Name)
		}
	}
	if len(namespaces) == 0 {
		namespaces = append(namespaces, namespace)
	}

	// Get all build configurations
	buildConfigList := make([]buildapi.BuildConfig, 0)
	for _, namespace := range namespaces {
		cfgList, err := osc.BuildConfigs(namespace).List(labels.Everything(), fields.Everything())
		if err != nil {
			return err
		}
		buildConfigList = append(buildConfigList, cfgList.Items...)
	}

	// Parse user input and validate specified image repository
	repos := make(map[string][]string)
	if !all && len(args) != 0 {
		name, specifiedTag, err := parseTag(args[0])
		if err != nil {
			return err
		}

		// Validate the specified image repository
		imgRepo, err := osc.ImageStreams(namespace).Get(name)
		if err != nil {
			return err
		}
		repo := join(namespace, name)

		// Validate specified tag
		tags := make([]string, 0)
		exists := false
		for tag := range imgRepo.Status.Tags {
			tags = append(tags, tag)
			if specifiedTag == tag {
				exists = true
			}
		}
		if !exists && !allTags {
			// The specified tag isn't a part of our image repository
			return fmt.Errorf("no tag %s exists in %s", specifiedTag, repo)
		} else if !allTags {
			// Use only the specified tag
			tags = []string{specifiedTag}
		}

		// Set the specified repo as the only one to output dependencies for
		repos[repo] = tags
	} else {
		repos = getRepos(buildConfigList)
	}

	if len(repos) == 0 {
		return fmt.Errorf("no image repository available for building its dependency tree")
	}

	output := cmdutil.GetFlagString(cmd, "output")
	for repo, tags := range repos {
		for _, tag := range tags {
			glog.V(4).Infof("Checking dependencies of repo %s tag %s", repo, tag)
			root, err := findRepoDeps(repo, tag, buildConfigList)
			if err != nil {
				return err
			}

			// Check if the given image repository doesn't have any dependencies
			if treeSize(root) < 2 {
				glog.Infof("%s:%s has no dependencies\n", root.FullName, tag)
				continue
			}

			switch output {
			case "json":
				jsonDump, err := json.MarshalIndent(root, "", "\t")
				if err != nil {
					return err
				}
				fmt.Println(string(jsonDump))
			case "dot":
				g := dot.NewGraph()
				_, name, err := split(repo)
				if err != nil {
					return err
				}
				graphName := validDOT(name)
				g.SetName(graphName)
				// Directed graph since we illustrate dependencies
				g.SetDir(true)
				// Explicitly allow multiple pairs of edges between
				// the same pair of nodes
				g.SetStrict(false)
				out, err := dotDump(root, g, graphName)
				if err != nil {
					return err
				}
				fmt.Println(out)
			case "ast":
				fmt.Println(root)
			default:
				return cmdutil.UsageError(cmd, "Wrong output format specified: %s", output)
			}
		}
	}
	return nil
}

// getRepos iterates over a given set of build configurations
// and extracts all the image repositories which trigger a
// build when the image repository is updated
func getRepos(configs []buildapi.BuildConfig) map[string][]string {
	glog.V(1).Infof("Scanning buildconfigs")
	avoidDuplicates := make(map[string][]string)
	for _, cfg := range configs {
		glog.V(1).Infof("Scanning buildconfig %v", cfg)
		for _, tr := range cfg.Triggers {
			glog.V(1).Infof("Scanning trigger %v", tr)
			from := buildutil.GetImageStreamForStrategy(&cfg)
			glog.V(1).Infof("Strategy from= %v", from)
			if tr.ImageChange != nil && from != nil && from.Name != "" {
				glog.V(1).Infof("found ICT with from %s kind %s", from.Name, from.Kind)
				var name, tag string
				switch from.Kind {
				case "ImageStreamTag":
					bits := strings.Split(from.Name, ":")
					name = bits[0]
					tag = bits[1]
				default:
					// ImageStreamImage and DockerImage are never updated and so never
					// trigger builds.
					continue
				}
				var repo string
				switch from.Namespace {
				case "":
					repo = join(cfg.Namespace, name)
				default:
					repo = join(from.Namespace, name)
				}

				uniqueTag := true
				for _, prev := range avoidDuplicates[repo] {
					if prev == tag {
						uniqueTag = false
						break
					}
				}
				glog.V(1).Infof("checking unique tag %v %s", uniqueTag, tag)
				if uniqueTag {
					avoidDuplicates[repo] = append(avoidDuplicates[repo], tag)
				}
			}
		}
	}

	return avoidDuplicates
}

// findRepoDeps accepts an image repository and a list of build
// configurations and returns the dependency tree of the specified
// image repository
func findRepoDeps(repo, tag string, buildConfigList []buildapi.BuildConfig) (*ImageRepo, error) {
	root := &ImageRepo{
		FullName: repo,
		Tags:     []string{tag},
	}

	namespace, name, err := split(repo)
	if err != nil {
		return nil, err
	}

	// Search all build configurations in order to find the image
	// repositories depending on the specified image repository
	var childNamespace, childName, childTag string
	for _, cfg := range buildConfigList {
		for _, tr := range cfg.Triggers {
			from := buildutil.GetImageStreamForStrategy(&cfg)
			if from == nil || from.Kind != "ImageStreamTag" || tr.ImageChange == nil {
				continue
			}
			// Setup zeroed fields to their default values
			if from.Namespace == "" {
				from.Namespace = cfg.Namespace
			}
			fromTag := strings.Split(from.Name, ":")[1]
			parentRepo := namespace + "/" + name + ":" + tag
			if buildutil.NameFromImageStream("", from, fromTag) == parentRepo {
				// Either To & Tag or DockerImageReference will be used as output
				if cfg.Parameters.Output.To != nil && cfg.Parameters.Output.To.Name != "" {
					childName = cfg.Parameters.Output.To.Name
					childTag = cfg.Parameters.Output.Tag
					if cfg.Parameters.Output.To.Namespace != "" {
						childNamespace = cfg.Parameters.Output.To.Namespace
					} else {
						childNamespace = cfg.Namespace
					}
				} else {
					ref, err := imageapi.ParseDockerImageReference(cfg.Parameters.Output.DockerImageReference)
					if err != nil {
						return nil, err
					}
					childName = ref.Name
					childTag = ref.Tag
					childNamespace = cfg.Namespace
				}

				childRepo := join(childNamespace, childName)

				// Build all children and their dependency trees recursively
				child, err := findRepoDeps(childRepo, childTag, buildConfigList)
				if err != nil {
					return nil, err
				}

				// Add edge between root and child
				cfgFullName := join(cfg.Namespace, cfg.Name)
				root.Edges = append(root.Edges, NewEdge(cfgFullName, child.FullName))

				// If the child depends on root via more than one tag, we have to make sure
				// that only one single instance of the child will make it into root.Children
				cont := false
				for _, repo := range root.Children {
					if repo.FullName == child.FullName {
						// Just pass the tag along and discard the current child
						repo.Tags = append(repo.Tags, child.Tags...)
						cont = true
						break
					}
				}
				if cont {
					// Do not append this child in root.Children. It's already in there
					continue
				}

				root.Children = append(root.Children, child)
			}
		}
	}
	return root, nil
}

var once sync.Once

// dotDump dumps the given image repository tree in DOT syntax
func dotDump(root *ImageRepo, g *dot.Graph, graphName string) (string, error) {
	if root == nil {
		return "", nil
	}

	// Add current node
	rootNamespace, rootName, err := split(root.FullName)
	if err != nil {
		return "", err
	}
	attrs := make(map[string]string)
	for _, tag := range root.Tags {
		setTag(tag, attrs)
	}
	var tag string
	// Inject tag into root's name
	once.Do(func() {
		tag = root.Tags[0]
	})
	setLabel(rootName, rootNamespace, attrs, tag)
	rootName = validDOT(rootName)
	g.AddNode(graphName, rootName, attrs)

	// Add edges between current node and its children
	for _, child := range root.Children {
		for _, edge := range root.Edges {
			if child.FullName == edge.To {
				_, childName, err := split(child.FullName)
				if err != nil {
					return "", err
				}
				childName = validDOT(childName)
				edgeNamespace, edgeName, err := split(edge.FullName)
				if err != nil {
					return "", err
				}
				edgeName = validDOT(edgeName)

				edgeAttrs := make(map[string]string)
				setLabel(edgeName, edgeNamespace, edgeAttrs, "")
				g.AddEdge(rootName, childName, true, edgeAttrs)
			}
		}
		// Recursively add every child and their children as nodes
		if _, err := dotDump(child, g, graphName); err != nil {
			return "", err
		}
	}

	dotOutput := g.String()

	// Parse DOT output for validation
	if _, err := dot.Parse([]byte(dotOutput)); err != nil {
		return "", fmt.Errorf("cannot parse DOT output: %v", err)
	}

	return dotOutput, nil
}
