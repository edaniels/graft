import * as vscode from "vscode";
import { GraftCli, ConnectionInfo } from "./graft-cli";
import { ConnectionProvider } from "./connection-provider";
import { StatusBar } from "./status-bar";
import { GraftTerminalProfileProvider } from "./terminal-profile";

export function activate(context: vscode.ExtensionContext) {
  const cli = new GraftCli();
  const provider = new ConnectionProvider(cli);
  const terminalProfile = new GraftTerminalProfileProvider(provider, cli);

  context.subscriptions.push(
    provider,
    new StatusBar(provider),
    vscode.window.registerTerminalProfileProvider(
      "graft.terminal",
      terminalProfile
    ),
    vscode.commands.registerCommand("graft.selectConnection", () =>
      selectConnection(provider, cli)
    ),
    vscode.commands.registerCommand("graft.openTerminal", () =>
      terminalProfile.openTerminal()
    ),
    vscode.workspace.onDidChangeWorkspaceFolders(() =>
      reportWorkspaceCwd(cli)
    )
  );

  reportWorkspaceCwd(cli).then(() => provider.refresh());
}

async function reportWorkspaceCwd(cli: GraftCli): Promise<void> {
  const folders = vscode.workspace.workspaceFolders;
  if (!folders?.length) {
    return;
  }
  try {
    await cli.reportCwd(folders[0].uri.fsPath);
  } catch (err) {
    console.error("[graft] failed to report CWD:", err);
  }
}

async function selectConnection(
  provider: ConnectionProvider,
  cli: GraftCli
): Promise<void> {
  if (!provider.daemonRunning) {
    vscode.window.showErrorMessage(
      "Graft daemon is not running. Start it with: graft daemon"
    );
    return;
  }

  await provider.refresh();
  const connections = provider.getConnections();

  if (connections.length === 0) {
    vscode.window.showInformationMessage("No graft connections available.");
    return;
  }

  const items = connections.map((c) => ({
    label: c.name,
    description: c.safeDestination,
    detail: formatConnectionDetail(c),
    connection: c,
  }));

  const selected = await vscode.window.showQuickPick(items, {
    placeHolder: "Select a graft connection to pin",
  });

  if (!selected) {
    return;
  }

  try {
    await cli.pinConnection(selected.connection.name);
    await provider.refresh();
  } catch (err) {
    vscode.window.showErrorMessage(
      `Failed to pin connection: ${err instanceof Error ? err.message : err}`
    );
  }
}

function formatConnectionDetail(c: ConnectionInfo): string {
  const state = c.state.replace("CONNECTION_STATE_", "").toLowerCase();
  const parts = [state];
  if (c.current) parts.push("current");
  if (c.stateReason) parts.push(c.stateReason);
  return parts.join(" - ");
}

export function deactivate() {}
