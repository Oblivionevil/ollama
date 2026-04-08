import { useEffect, useMemo, useRef } from "react";
import { useModels } from "./useModels";
import { useChat } from "./useChats";
import { useSettings } from "./useSettings.ts";
import { Model } from "@/gotypes";

export function useSelectedModel(currentChatId?: string, searchQuery?: string) {
  const { settings, setSettings } = useSettings();
  const { data: catalogModels = [], isLoading: isCatalogLoading } = useModels("");
  const { data: filteredModels = [], isLoading: isFilteredLoading } = useModels(
    searchQuery || "",
  );
  const { data: chatData, isLoading: isChatLoading } = useChat(
    currentChatId && currentChatId !== "new" ? currentChatId : "",
  );

  // Track which chat we've already restored the model for
  const restoredChatRef = useRef<string | null>(null);

  const selectedModel: Model | null = useMemo(() => {
    return (
      catalogModels.find((model) => model.model === settings.selectedModel) ||
      null
    );
  }, [catalogModels, settings.selectedModel]);

  // Set model from chat history when chat data loads
  useEffect(() => {
    // Only run this effect if we have a valid currentChatId
    if (!currentChatId || currentChatId === "new") {
      return;
    }

    if (
      chatData?.chat?.messages &&
      !isCatalogLoading &&
      !isChatLoading &&
      restoredChatRef.current !== currentChatId
    ) {
      // Find the most recent model used in this chat
      const messages = [...chatData.chat.messages].reverse();
      for (const message of messages) {
        if (typeof message.model === "string" && message.model) {
          const chatModelName = message.model;

          if (
            catalogModels.some((model) => model.model === chatModelName) &&
            chatModelName !== settings.selectedModel
          ) {
            setSettings({ SelectedModel: chatModelName });
          }

          // Mark this chat as restored
          restoredChatRef.current = currentChatId;
          return;
        }
      }
      // Mark this chat as processed even if no model was found
      restoredChatRef.current = currentChatId;
    }
  }, [
    currentChatId,
    chatData,
    isCatalogLoading,
    isChatLoading,
    catalogModels,
    settings.selectedModel,
    setSettings,
  ]);

  // On initial load, if no model is selected, set default model
  useEffect(() => {
    if (isCatalogLoading || catalogModels.length === 0 || selectedModel) {
      return;
    }

    setSettings({ SelectedModel: catalogModels[0].model });
  }, [catalogModels, isCatalogLoading, selectedModel, setSettings]);

  return {
    selectedModel,
    setSettings,
    models: searchQuery ? filteredModels : catalogModels,
    loading: isCatalogLoading || isFilteredLoading,
  };
}
