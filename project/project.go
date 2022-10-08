package project

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sync"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/moby/buildkit/client/llb"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"go.dagger.io/dagger/core/filesystem"
	"go.dagger.io/dagger/router"
	"gopkg.in/yaml.v2"
)

const (
	schemaPath     = "/schema.graphql"
	entrypointPath = "/entrypoint"

	DaggerSockName = "dagger-sock"
	daggerSockPath = "/dagger.sock"

	fsMountPath  = "/mnt"
	tmpMountPath = "/tmp"

	inputMountPath = "/inputs"
	inputFile      = "/dagger.json"

	outputMountPath = "/outputs"
	outputFile      = "/dagger.json"
)

type State struct {
	config     projectConfig
	workdir    *filesystem.Filesystem
	configPath string

	schema     string
	schemaOnce sync.Once

	dependencies     []*State
	dependenciesOnce sync.Once

	extensionsOnce sync.Once

	resolvers     router.Resolvers
	resolversOnce sync.Once
}

func Load(
	ctx context.Context,
	workdir *filesystem.Filesystem,
	configPath string,
	cache map[string]*State,
	cacheMu *sync.RWMutex,
	gw bkgw.Client,
) (*State, error) {
	cfgBytes, err := workdir.ReadFile(ctx, gw, configPath)
	if err != nil {
		return nil, err
	}
	s := &State{
		workdir:    workdir,
		configPath: configPath,
		resolvers:  make(router.Resolvers),
	}
	if err := yaml.UnmarshalStrict(cfgBytes, &s.config); err != nil {
		return nil, err
	}

	if s.config.Name == "" {
		return nil, fmt.Errorf("project name must be set")
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	existing, ok := cache[s.config.Name]
	if ok {
		return existing, nil
	}
	cache[s.config.Name] = s
	return s, nil
}

func (p *State) Name() string {
	return p.config.Name
}

func (p *State) Schema(ctx context.Context, gw bkgw.Client, platform specs.Platform, sshAuthSockID string) (string, error) {
	var rerr error
	p.schemaOnce.Do(func() {
		sourceSchemas := make([]router.LoadedSchema, len(p.config.Extensions))
		extensions, err := p.Extensions(ctx, gw, platform, sshAuthSockID)
		if err != nil {
			rerr = err
			return
		}
		for i, ext := range extensions {
			sourceSchemas[i] = router.StaticSchema(router.StaticSchemaParams{
				Schema: ext.Schema,
			})
		}
		p.schema = router.MergeLoadedSchemas(p.config.Name, sourceSchemas...).Schema()
	})
	return p.schema, rerr
}

func (p *State) Dependencies(
	ctx context.Context,
	cache map[string]*State,
	cacheMu *sync.RWMutex,
	gw bkgw.Client,
	platform specs.Platform,
	sshAuthSockID string,
) ([]*State, error) {
	var rerr error
	p.dependenciesOnce.Do(func() {
		p.dependencies = make([]*State, len(p.config.Dependencies))
		for i, dep := range p.config.Dependencies {
			switch {
			case dep.Local != "":
				depConfigPath := filepath.ToSlash(filepath.Join(filepath.Dir(p.configPath), dep.Local))
				depState, err := Load(ctx, p.workdir, depConfigPath, cache, cacheMu, gw)
				if err != nil {
					rerr = err
					return
				}
				p.dependencies[i] = depState
			case dep.Git != nil:
				var opts []llb.GitOption
				if sshAuthSockID != "" {
					opts = append(opts, llb.MountSSHSock(sshAuthSockID))
				}
				gitFS, err := filesystem.FromState(ctx, llb.Git(dep.Git.Remote, dep.Git.Ref, opts...), platform)
				if err != nil {
					rerr = err
					return
				}
				depState, err := Load(ctx, gitFS, dep.Git.Path, cache, cacheMu, gw)
				if err != nil {
					rerr = err
					return
				}
				p.dependencies[i] = depState
			}
		}
	})
	return p.dependencies, rerr
}

func (p *State) Extensions(ctx context.Context, gw bkgw.Client, platform specs.Platform, sshAuthSockID string) ([]*Extension, error) {
	var rerr error
	p.extensionsOnce.Do(func() {
		for i, ext := range p.config.Extensions {
			// first try to load a hardcoded schema
			// TODO: remove this once all extensions migrate to code-first
			schemaBytes, err := p.workdir.ReadFile(ctx, gw, path.Join(path.Dir(p.configPath), ext.Path, schemaPath))
			if err == nil {
				p.config.Extensions[i].Schema = string(schemaBytes)
				continue
			}

			// otherwise go ask the extension for its schema
			runtimeFS, err := p.Runtime(ctx, ext, gw, platform, sshAuthSockID)
			if err != nil {
				rerr = err
				return
			}
			fsState, err := runtimeFS.ToState()
			if err != nil {
				rerr = err
				return
			}

			workdirSt, err := p.workdir.ToState()
			if err != nil {
				rerr = err
				return
			}
			st := fsState.Run(
				llb.Args([]string{entrypointPath, "-schema"}),
				llb.AddMount("/src", workdirSt, llb.Readonly),
				llb.ReadonlyRootFS(),
			)
			outputMnt := st.AddMount(outputMountPath, llb.Scratch())
			outputDef, err := outputMnt.Marshal(ctx, llb.Platform(platform))
			if err != nil {
				rerr = err
				return
			}
			res, err := gw.Solve(ctx, bkgw.SolveRequest{
				Definition: outputDef.ToPB(),
			})
			if err != nil {
				rerr = err
				return
			}
			ref, err := res.SingleRef()
			if err != nil {
				rerr = err
				return
			}
			outputBytes, err := ref.ReadFile(ctx, bkgw.ReadRequest{
				Filename: "/schema.graphql",
			})
			if err != nil {
				rerr = err
				return
			}

			ext.Schema = string(outputBytes)
		}
	})
	return p.config.Extensions, rerr
}

func (p *State) Scripts() []*Script {
	return p.config.Scripts
}

func (p *State) Resolvers(ctx context.Context, gw bkgw.Client, platform specs.Platform, sshAuthSockID string) (router.Resolvers, error) {
	var rerr error
	p.resolversOnce.Do(func() {
		extensions, err := p.Extensions(ctx, gw, platform, sshAuthSockID)
		if err != nil {
			rerr = err
			return
		}
		for _, ext := range extensions {
			runtimeFS, err := p.Runtime(ctx, ext, gw, platform, sshAuthSockID)
			if err != nil {
				rerr = err
				return
			}
			doc, err := parser.Parse(parser.ParseParams{Source: ext.Schema})
			if err != nil {
				rerr = err
				return
			}
			for _, def := range doc.Definitions {
				var obj *ast.ObjectDefinition

				if def, ok := def.(*ast.ObjectDefinition); ok {
					obj = def
				}

				if def, ok := def.(*ast.TypeExtensionDefinition); ok {
					obj = def.Definition
				}

				if obj == nil {
					continue
				}

				objResolver := router.ObjectResolver{}
				p.resolvers[obj.Name.Value] = objResolver
				for _, field := range obj.Fields {
					objResolver[field.Name.Value] = p.resolver(runtimeFS, ext.SDK, gw, platform)
				}
			}
		}
	})
	return p.resolvers, rerr
}

func (p *State) resolver(runtimeFS *filesystem.Filesystem, sdk string, gw bkgw.Client, platform specs.Platform) graphql.FieldResolveFn {
	return router.ToResolver(func(ctx *router.Context, parent any, args any) (any, error) {
		pathArray := ctx.ResolveParams.Info.Path.AsArray()
		name := fmt.Sprintf("%+v", pathArray)

		resolverName := fmt.Sprintf("%s.%s", ctx.ResolveParams.Info.ParentType.Name(), ctx.ResolveParams.Info.FieldName)
		inputMap := map[string]interface{}{
			"resolver": resolverName,
			"args":     args,
			"parent":   parent,
		}
		inputBytes, err := json.Marshal(inputMap)
		if err != nil {
			return nil, err
		}
		input := llb.Scratch().File(llb.Mkfile(inputFile, 0644, inputBytes))

		fsState, err := runtimeFS.ToState()
		if err != nil {
			return nil, err
		}

		workdirSt, err := p.workdir.ToState()
		if err != nil {
			return nil, err
		}

		st := fsState.Run(
			llb.Args([]string{entrypointPath}),
			llb.AddSSHSocket(
				llb.SSHID(DaggerSockName),
				llb.SSHSocketTarget(daggerSockPath),
			),
			llb.AddMount(inputMountPath, input, llb.Readonly),
			llb.AddMount(tmpMountPath, llb.Scratch(), llb.Tmpfs()),
			llb.ReadonlyRootFS(),
		)

		// TODO:
		if sdk == "go" {
			st.AddMount("/src", workdirSt, llb.Readonly) // TODO: not actually needed here, just makes go server code easier at moment
		}

		// TODO: /mnt should maybe be configurable?
		for path, fsid := range collectFSPaths(ctx.ResolveParams.Args, fsMountPath, nil) {
			fs := filesystem.New(fsid)
			fsState, err := fs.ToState()
			if err != nil {
				return nil, err
			}
			// TODO: it should be possible for this to be outputtable by the action; the only question
			// is how to expose that ability in a non-confusing way, just needs more thought
			st.AddMount(path, fsState, llb.ForceNoOutput)
		}

		// Mount in the parent type if it is a Filesystem
		// FIXME:(sipsma) got to be a better way than string matching parent type... But not easy
		// to just use go type matching because the parent result may be a Filesystem struct or
		// an untyped map[string]interface{}.
		if ctx.ResolveParams.Info.ParentType.Name() == "Filesystem" {
			var parentFS filesystem.Filesystem
			bytes, err := json.Marshal(parent)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(bytes, &parentFS); err != nil {
				return nil, err
			}
			fsState, err := parentFS.ToState()
			if err != nil {
				return nil, err
			}
			// FIXME:(sipsma) not a good place to hardcode mounting this in, same as mounting in resolver args
			st.AddMount("/mnt/.parent", fsState, llb.ForceNoOutput)
		}

		outputMnt := st.AddMount(outputMountPath, llb.Scratch())
		outputDef, err := outputMnt.Marshal(ctx, llb.Platform(platform), llb.WithCustomName(name))
		if err != nil {
			return nil, err
		}

		res, err := gw.Solve(ctx, bkgw.SolveRequest{
			Definition: outputDef.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		outputBytes, err := ref.ReadFile(ctx, bkgw.ReadRequest{
			Filename: outputFile,
		})
		if err != nil {
			return nil, err
		}
		var output interface{}
		if err := json.Unmarshal(outputBytes, &output); err != nil {
			return nil, fmt.Errorf("failed to unmarshal output: %w", err)
		}
		return output, nil
	})
}

func collectFSPaths(arg interface{}, curPath string, fsPaths map[string]filesystem.FSID) map[string]filesystem.FSID {
	if fsPaths == nil {
		fsPaths = make(map[string]filesystem.FSID)
	}

	switch arg := arg.(type) {
	case filesystem.FSID:
		// TODO: make sure there can't be any shenanigans with args named e.g. ../../../foo/bar
		fsPaths[curPath] = arg
	case map[string]interface{}:
		for k, v := range arg {
			fsPaths = collectFSPaths(v, filepath.Join(curPath, k), fsPaths)
		}
	case []interface{}:
		for i, v := range arg {
			// TODO: path format technically works but weird as hell, gotta be a better way
			fsPaths = collectFSPaths(v, fmt.Sprintf("%s/%d", curPath, i), fsPaths)
		}
	}
	return fsPaths
}