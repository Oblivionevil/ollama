export async function openExternalUrl(url: string) {
  if (!url) {
    return;
  }

  if (typeof window.openExternal === "function") {
    try {
      await window.openExternal(url);
      return;
    } catch (error) {
      console.error("Failed to open external URL via desktop binding:", error);
    }
  }

  window.open(url, "_blank", "noopener,noreferrer");
}