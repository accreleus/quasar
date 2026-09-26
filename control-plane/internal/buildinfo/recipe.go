package buildinfo

// RecipeRevision is the recipe revision this control-plane image needs (ADR 0008): the
// container shape a recovery actor renders it with. deploy/build-images.sh stamps it as
// the image label org.quasar.recipe by reading the line below, so keep its exact shape.
// Bump it only when the control plane starts needing a new mount, environment input,
// port, device or capability, together with the actor's recipe book.
const RecipeRevision = 2
