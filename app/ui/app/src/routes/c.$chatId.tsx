import { createFileRoute } from "@tanstack/react-router";
import { useChat } from "@/hooks/useChats";
import Chat from "@/components/Chat";
import { getChat } from "@/api";
import { SidebarLayout } from "@/components/layout/layout";
import { ChatSidebar } from "@/components/ChatSidebar";

export const Route = createFileRoute("/c/$chatId")({
  component: RouteComponent,
  loader: async ({ context, params }) => {
    if (params.chatId !== "new") {
      context.queryClient.ensureQueryData({
        queryKey: ["chat", params.chatId],
        queryFn: () => getChat(params.chatId),
        staleTime: 1500,
      });
    }
  },
});

function RouteComponent() {
  const { chatId } = Route.useParams();

  const {
    data: chatData,
    isLoading: chatLoading,
    error: chatError,
  } = useChat(chatId === "new" ? "" : chatId);

  if (chatId === "new") {
    return (
      <SidebarLayout sidebar={<ChatSidebar currentChatId={chatId} />}>
        <Chat chatId={chatId} />
      </SidebarLayout>
    );
  }

  if (chatLoading) {
    return (
      <SidebarLayout sidebar={<ChatSidebar currentChatId={chatId} />}>
        <div className="p-4">Loading chat...</div>
      </SidebarLayout>
    );
  }

  if (chatError) {
    return (
      <SidebarLayout sidebar={<ChatSidebar currentChatId={chatId} />}>
        <div className="p-4 text-red-500">Error loading chat</div>
      </SidebarLayout>
    );
  }

  if (!chatData) {
    return (
      <SidebarLayout sidebar={<ChatSidebar currentChatId={chatId} />}>
        <div className="p-4">Chat not found</div>
      </SidebarLayout>
    );
  }

  return (
    <SidebarLayout sidebar={<ChatSidebar currentChatId={chatId} />}>
      <Chat chatId={chatId} />
    </SidebarLayout>
  );
}
