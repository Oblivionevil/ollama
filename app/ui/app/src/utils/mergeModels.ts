import { Model } from "@/gotypes";

function alphabeticalSort(a: Model, b: Model): number {
  return a.model.toLowerCase().localeCompare(b.model.toLowerCase());
}

function dedupeModels(models: Model[]): Model[] {
  const seen = new Set<string>();

  return models.filter((model) => {
    const key = model.model.toLowerCase();
    if (seen.has(key)) {
      return false;
    }

    seen.add(key);
    return true;
  });
}

export function mergeModels(
  models: Model[],
  hideRemoteModels: boolean = false,
): Model[] {
  return dedupeModels(
    (models || [])
      .filter((model) => Boolean(model?.model))
      .filter((model) => !hideRemoteModels || !model.isRemote())
      .sort((left, right) => {
        if (left.isRemote() !== right.isRemote()) {
          return left.isRemote() ? -1 : 1;
        }

        return alphabeticalSort(left, right);
      }),
  );
}
