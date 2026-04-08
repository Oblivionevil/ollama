import { describe, it, expect } from "vitest";
import { Model } from "@/gotypes";
import { mergeModels } from "@/utils/mergeModels";
import "@/api";

function buildModel(modelName: string, options?: { remote?: boolean }): Model {
  const model = new Model({ model: modelName });
  model.remote = options?.remote ?? false;
  return model;
}

describe("mergeModels", () => {
  it("sorts remote models before local models", () => {
    const merged = mergeModels([
      buildModel("llama3:latest"),
      buildModel("copilot/gpt-5", { remote: true }),
      buildModel("mistral:latest"),
      buildModel("copilot/gpt-4.1", { remote: true }),
    ]);

    expect(merged.map((model) => model.model)).toEqual([
      "copilot/gpt-4.1",
      "copilot/gpt-5",
      "llama3:latest",
      "mistral:latest",
    ]);
  });

  it("filters remote models when remote access is disabled", () => {
    const merged = mergeModels(
      [
        buildModel("copilot/gpt-5", { remote: true }),
        buildModel("llama3:latest"),
        buildModel("mistral:latest"),
      ],
      true,
    );

    expect(merged.map((model) => model.model)).toEqual([
      "llama3:latest",
      "mistral:latest",
    ]);
  });

  it("deduplicates models case-insensitively", () => {
    const merged = mergeModels([
      buildModel("llama3:latest"),
      buildModel("LLaMA3:latest"),
      buildModel("copilot/gpt-5", { remote: true }),
      buildModel("COPILOT/GPT-5", { remote: true }),
    ]);

    expect(merged.map((model) => model.model)).toEqual([
      "copilot/gpt-5",
      "llama3:latest",
    ]);
  });

  it("handles empty input", () => {
    expect(mergeModels([])).toEqual([]);
  });
});
