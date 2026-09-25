//! Every path that builds a platform image stamps `org.quasar.recipe` from the one
//! derivation, `deploy/lib/recipe-revision.sh` (ADR 0008): `deploy/build-images.sh` and the
//! Images workflow. The contract requires the label, and a recovery actor refuses an image
//! without it, so a build path that forgets it ships an image nothing can install.

use std::path::Path;

use serde_yaml::Value;

fn repo(rel: &str) -> String {
    let path = Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .join(rel);
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

/// The step list of one workflow job.
fn steps<'a>(workflow: &'a Value, job: &str) -> &'a Vec<Value> {
    workflow["jobs"][job]["steps"]
        .as_sequence()
        .unwrap_or_else(|| panic!("images.yml has no job {job} with steps"))
}

#[test]
fn the_images_workflow_labels_each_platform_image_with_its_recipe_revision() {
    let workflow: Value = serde_yaml::from_str(&repo(".github/workflows/images.yml")).unwrap();
    for (job, dockerfile, role) in [
        (
            "build-control-plane",
            "deploy/Dockerfile.control.prod",
            "control",
        ),
        ("build-node-agent", "deploy/Dockerfile.vulkan", "runtime"),
    ] {
        let steps = steps(&workflow, job);
        let recipe = steps
            .iter()
            .find(|s| s["id"].as_str() == Some("recipe"))
            .unwrap_or_else(|| panic!("{job}: no step with id `recipe`"));
        let run = recipe["run"].as_str().unwrap_or("");
        assert!(
            run.contains(&format!("deploy/lib/recipe-revision.sh {role}")),
            "{job}: the recipe step must call deploy/lib/recipe-revision.sh {role}: {run:?}"
        );
        let build = steps
            .iter()
            .find(|s| s["with"]["file"].as_str() == Some(dockerfile))
            .unwrap_or_else(|| panic!("{job}: no build step for {dockerfile}"));
        let labels = build["with"]["labels"].as_str().unwrap_or("");
        assert!(
            labels
                .lines()
                .any(|l| l.trim() == "org.quasar.recipe=${{ steps.recipe.outputs.revision }}"),
            "{job}: the build must carry org.quasar.recipe from the recipe step: {labels:?}"
        );
    }
}

#[test]
fn build_images_stamps_the_label_through_the_same_helper() {
    let script = repo("deploy/build-images.sh");
    assert!(script.contains("lib/recipe-revision.sh"));
    assert!(script.contains("--label \"org.quasar.recipe=$ROLE_RECIPE\""));
}
