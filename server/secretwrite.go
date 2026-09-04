/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// clusterSecrets writes the values a user typed, into the cluster this server
// runs in.
//
// # A merge patch, and why it is not a server-side apply
//
// Everything else this package writes is applied. This is patched, because of a
// permission it deliberately does not have: the control plane may create and
// patch a Secret and may not read one. An apply has to send the whole object,
// so keeping a value it is not changing would mean knowing that value — and the
// whole point of not holding `get` is that it never does. A merge patch changes
// the keys it names, deletes the keys it names as null, and leaves everything
// else alone without ever having seen it.
//
// The same asymmetry is what makes "a secret's value is never in an API
// response" structural instead of a promise: there is no code path that could
// return one, because there is no code path that could fetch one.
type clusterSecrets struct{ client client.Client }

// secretDataField is the one field these patches touch, named because a typo in
// it would produce a patch the API server accepts and that changes nothing.
const secretDataField = "data"

// Put sets the keys in set and removes the keys in remove.
//
// Creating on NotFound rather than checking first, which is the same shape the
// permission forces: a get would be a read, and a create-then-patch race
// between two replicas resolves itself — whichever loses gets AlreadyExists and
// its patch lands on the object the winner made.
func (s clusterSecrets) Put(
	ctx context.Context, namespace, name string, set map[string]string, remove []string,
) error {
	if len(set) == 0 && len(remove) == 0 {
		return nil
	}

	data := make(map[string]any, len(set)+len(remove))
	for k, v := range set {
		// base64 because this is the `data` field. stringData would be the
		// short way and it is write-only: the API server converts it, so a
		// patch that sent it would name a field the stored object does not
		// have.
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	for _, k := range remove {
		// null is how a merge patch says "delete this key".
		data[k] = nil
	}

	patch, err := json.Marshal(map[string]any{
		"metadata":      map[string]any{"labels": asAny(userSecretLabels())},
		secretDataField: data,
	})
	if err != nil {
		return fmt.Errorf("building the secret patch: %w", err)
	}

	obj := userSecretObject(namespace, name)
	err = s.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Nothing to create when the only instruction was a removal: a Secret that
	// does not exist already holds none of the keys being removed.
	if len(set) == 0 {
		return nil
	}
	fresh := userSecretObject(namespace, name)
	fresh.SetLabels(userSecretLabels())
	created := make(map[string]any, len(set))
	for k, v := range set {
		created[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	if err := unstructured.SetNestedMap(fresh.Object, created, secretDataField); err != nil {
		return fmt.Errorf("building the secret: %w", err)
	}
	fresh.Object["type"] = "Opaque"
	if err := s.client.Create(ctx, fresh); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil
	}
	// Another replica created it between the patch and this create. Patch the
	// object that now exists rather than reporting a conflict to somebody who
	// pressed save once.
	return s.client.Patch(ctx, userSecretObject(namespace, name),
		client.RawPatch(types.MergePatchType, patch))
}

func userSecretObject(namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("Secret")
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

// userSecretLabels say who wrote this and what it is for. They are not
// decoration: this object is the one thing in a tenant namespace that no
// manifest describes, so a person looking for it has nothing else to go on.
func userSecretLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": managedByDamga,
		"damga.co/settings":            "user-supplied",
	}
}

// asAny is the same map, for a JSON document rather than an object.
func asAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// clusterSecrets builds the writer, or says why there is none.
func (o Options) clusterSecretWriter() (SecretWriter, error) {
	restCfg := o.RestConfig
	if restCfg == nil {
		var err error
		if restCfg, err = ctrl.GetConfig(); err != nil {
			return nil, fmt.Errorf("holding a secret value needs a cluster: %w", err)
		}
	}
	c, err := client.New(restCfg, client.Options{Scheme: runtime.NewScheme()})
	if err != nil {
		return nil, fmt.Errorf("building the secret client: %w", err)
	}
	return clusterSecrets{client: c}, nil
}
