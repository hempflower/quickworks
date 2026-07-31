import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";

const root = document.querySelector("[data-workspace-status]");

if (root) {
  const workspaceID = root.dataset.workspaceId;
  const buildID = root.dataset.buildId;
  const terminalElement = document.querySelector("#build-terminal");
  const statusElement = document.querySelector("#workspace-status");
  const terminal = terminalElement
    ? new Terminal({
        convertEol: true,
        cursorBlink: false,
        disableStdin: true,
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
        fontSize: 13,
        theme: {
          background: "#0f172a",
          foreground: "#e2e8f0",
        },
      })
    : null;

  terminal?.open(terminalElement);
  terminal?.writeln("Waiting for build output…");

  if (buildID && terminal) {
    const stream = new EventSource(`/api/builds/${encodeURIComponent(buildID)}/logs`);
    stream.addEventListener("log", (event) => {
      const entry = JSON.parse(event.data);
      terminal.writeln(`\x1b[90m[${entry.level}]\x1b[0m ${entry.message}`);
    });
    stream.onerror = () => {
      terminal.writeln("\x1b[33mBuild log stream reconnecting…\x1b[0m");
    };
  }

  const checkWorkspace = async () => {
    try {
      const response = await fetch(
        `/api/workspaces/${encodeURIComponent(workspaceID)}/readiness`,
        { credentials: "same-origin" },
      );
      if (!response.ok) {
        return;
      }
      const workspace = await response.json();
      if (statusElement) {
        statusElement.textContent = `Workspace is ${workspace.observed_state}.`;
      }
      if (workspace.observed_state === "running" && workspace.ready) {
        window.location.reload();
      }
    } catch {
      // The next polling interval retries transient control-plane failures.
    }
  };

  void checkWorkspace();
  window.setInterval(checkWorkspace, 2000);
}
