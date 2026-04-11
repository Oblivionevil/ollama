import { createFileRoute, redirect } from "@tanstack/react-router";
import { getSettings } from "@/api";

export const Route = createFileRoute("/")({
  beforeLoad: async ({ context }) => {
    await context.queryClient.ensureQueryData({
      queryKey: ["settings"],
      queryFn: getSettings,
    });

    throw redirect({
      to: "/c/$chatId",
      params: { chatId: "new" },
      mask: {
        to: "/",
      },
    });
  },
});
