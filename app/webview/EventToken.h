#ifndef OLLAMA_APP_WEBVIEW_EVENTTOKEN_H
#define OLLAMA_APP_WEBVIEW_EVENTTOKEN_H

#ifndef DECLSPEC_XFGVIRT
#if defined(_CONTROL_FLOW_GUARD_XFG)
#define DECLSPEC_XFGVIRT(base, func) __declspec(xfg_virtual(base, func))
#else
#define DECLSPEC_XFGVIRT(base, func)
#endif
#endif

typedef struct EventRegistrationToken {
  __int64 value;
} EventRegistrationToken;

#endif