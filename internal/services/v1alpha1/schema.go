package v1alpha1

import (
	"context"
	"errors"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/spicedb/internal/datastore"
	"github.com/authzed/spicedb/internal/namespace"
	"github.com/authzed/spicedb/internal/services/serviceerrors"
	"github.com/authzed/spicedb/internal/sharederrors"
	v0 "github.com/authzed/spicedb/pkg/proto/authzed/api/v0"
	v1alpha1 "github.com/authzed/spicedb/pkg/proto/authzed/api/v1alpha1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/generator"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
)

type schemaServiceServer struct {
	v1alpha1.UnimplementedSchemaServiceServer

	ds datastore.Datastore
}

// NewSchemaServer returns an new instance of a server that implements
// authzed.api.v1alpha1.SchemaService.
func NewSchemaServer(ds datastore.Datastore) v1alpha1.SchemaServiceServer {
	return &schemaServiceServer{ds: ds}
}

func (ss *schemaServiceServer) ReadSchema(ctx context.Context, in *v1alpha1.ReadSchemaRequest) (*v1alpha1.ReadSchemaResponse, error) {
	if err := in.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid argument: %s", err)
	}

	var objectDefs []string
	for _, objectDefName := range in.GetObjectDefinitionsNames() {
		found, _, err := ss.ds.ReadNamespace(ctx, objectDefName)
		if err != nil {
			return nil, rewriteError(err)
		}

		objectDef, _ := generator.GenerateSource(found)
		objectDefs = append(objectDefs, objectDef)
	}

	return &v1alpha1.ReadSchemaResponse{
		ObjectDefinitions: objectDefs,
	}, nil
}

func (ss *schemaServiceServer) WriteSchema(ctx context.Context, in *v1alpha1.WriteSchemaRequest) (*v1alpha1.WriteSchemaResponse, error) {
	log.Trace().Str("schema", in.GetSchema()).Msg("requested Schema to be written")
	if err := in.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid argument: %s", err)
	}

	nsm, err := namespace.NewCachingNamespaceManager(ss.ds, 0, nil) // non-caching manager
	if err != nil {
		return nil, rewriteError(err)
	}

	inputSchema := compiler.InputSchema{
		Source:       input.InputSource("schema"),
		SchemaString: in.GetSchema(),
	}

	nsdefs, err := compiler.Compile([]compiler.InputSchema{inputSchema}, nil)
	if err != nil {
		return nil, rewriteError(err)
	}
	log.Trace().Interface("namespace definitions", nsdefs).Msg("compiled namespace definitions")

	for _, nsdef := range nsdefs {
		ts, err := namespace.BuildNamespaceTypeSystem(nsdef, nsm, nsdefs...)
		if err != nil {
			return nil, rewriteError(err)
		}

		if err := ts.Validate(ctx); err != nil {
			return nil, rewriteError(err)
		}

		if err := sanityCheckExistingRelationships(ctx, ss.ds, nsdef); err != nil {
			return nil, rewriteError(err)
		}
	}
	log.Trace().Interface("namespace definitions", nsdefs).Msg("validated namespace definitions")

	var names []string
	for _, nsdef := range nsdefs {
		if _, err := ss.ds.WriteNamespace(ctx, nsdef); err != nil {
			return nil, rewriteError(err)
		}

		names = append(names, nsdef.Name)
	}
	log.Trace().Interface("namespace definitions", nsdefs).Msg("wrote namespace definitions")

	return &v1alpha1.WriteSchemaResponse{
		ObjectDefinitionsNames: names,
	}, nil
}

// TODO(jzelinskie): figure how to deduplicate this code across v0 and v1 APIs.
func sanityCheckExistingRelationships(ctx context.Context, ds datastore.Datastore, nsdef *v0.NamespaceDefinition) error {
	// Ensure that the updated namespace does not break the existing tuple data.
	//
	// NOTE: We use the datastore here to read the namespace, rather than the namespace manager,
	// to ensure there is no caching being used.
	existing, revision, err := ds.ReadNamespace(ctx, nsdef.Name)
	if err != nil && !errors.As(err, &datastore.ErrNamespaceNotFound{}) {
		return err
	}

	diff, err := namespace.DiffNamespaces(existing, nsdef)
	if err != nil {
		return err
	}

	for _, delta := range diff.Deltas() {
		switch delta.Type {
		case namespace.RemovedRelation:
			err = errorIfTupleIteratorReturnsTuples(
				ds.QueryTuples(nsdef.Name, revision).WithRelation(delta.RelationName),
				ctx,
				"cannot delete Relation `%s` in Object Definition `%s`, as a Relationship exists under it", delta.RelationName, nsdef.Name)
			if err != nil {
				return err
			}

			// Also check for right sides of tuples.
			err = errorIfTupleIteratorReturnsTuples(
				ds.ReverseQueryTuplesFromSubjectRelation(nsdef.Name, delta.RelationName, revision),
				ctx,
				"cannot delete Relation `%s` in Object Definition `%s`, as a Relationship references it", delta.RelationName, nsdef.Name)
			if err != nil {
				return err
			}

		case namespace.RelationDirectTypeRemoved:
			err = errorIfTupleIteratorReturnsTuples(
				ds.ReverseQueryTuplesFromSubjectRelation(delta.DirectType.Namespace, delta.DirectType.Relation, revision).
					WithObjectRelation(nsdef.Name, delta.RelationName),
				ctx,
				"cannot remove allowed direct Relation `%s#%s` from Relation `%s` in Object Definition `%s`, as a Relationship exists with it",
				delta.DirectType.Namespace, delta.DirectType.Relation, delta.RelationName, nsdef.Name)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func errorIfTupleIteratorReturnsTuples(query datastore.CommonTupleQuery, ctx context.Context, message string, args ...interface{}) error {
	qy, err := query.Limit(1).Execute(ctx)
	if err != nil {
		return err
	}
	defer qy.Close()

	rt := qy.Next()
	if rt != nil {
		if qy.Err() != nil {
			return qy.Err()
		}

		return status.Errorf(codes.InvalidArgument, message, args...)
	}
	return nil
}

func rewriteError(err error) error {
	var nsNotFoundError sharederrors.UnknownNamespaceError = nil
	var errWithContext compiler.ErrorWithContext

	switch {
	case errors.As(err, &nsNotFoundError):
		return status.Errorf(codes.NotFound, "Object Definition `%s` not found", nsNotFoundError.NotFoundNamespaceName())
	case errors.As(err, &errWithContext):
		return status.Errorf(codes.InvalidArgument, "%s", err)
	case errors.As(err, &datastore.ErrReadOnly{}):
		return serviceerrors.ErrServiceReadOnly
	default:
		log.Err(err)
		return err
	}
}