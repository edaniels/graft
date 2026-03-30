import * as vscode from "vscode";
import { ConnectionProvider } from "./connection-provider";
import { ConnectionInfo, GraftCli } from "./graft-cli";

export class GraftTerminalProfileProvider
  implements vscode.TerminalProfileProvider
{
  constructor(
    private readonly provider: ConnectionProvider,
    private readonly cli: GraftCli
  ) {}

  async provideTerminalProfile(
    _token: vscode.CancellationToken
  ): Promise<vscode.TerminalProfile | undefined> {
    return this.resolve();
  }

  async openTerminal(): Promise<void> {
    const profile = await this.resolve();
    if (!profile) {
      return;
    }
    const terminal = vscode.window.createTerminal(profile.options);
    terminal.show();
  }

  private async resolve(): Promise<vscode.TerminalProfile | undefined> {
    if (!this.provider.daemonRunning) {
      vscode.window.showErrorMessage(
        "Graft daemon is not running. Start it with: graft daemon"
      );
      return undefined;
    }

    await this.provider.refresh();

    // If there's already a current connection (via pin or CWD), just open the shell
    const current = this.provider.currentConnection();
    if (current) {
      return await this.createProfile(current.name);
    }

    // No current connection - let the user pick
    const connections = this.provider.getConnections();
    if (connections.length === 0) {
      vscode.window.showInformationMessage("No graft connections available.");
      return undefined;
    }

    const picked = await this.pickConnection(connections);
    if (!picked) {
      return undefined;
    }

    // Pin the selected connection so the CLI resolves it
    await this.cli.pinConnection(picked.name);
    await this.provider.refresh();

    return await this.createProfile(picked.name);
  }

  private async pickConnection(
    connections: ConnectionInfo[]
  ): Promise<ConnectionInfo | undefined> {
    if (connections.length === 1) {
      return connections[0];
    }

    const items = connections.map((c) => ({
      label: c.name,
      description: c.safeDestination,
      connection: c,
    }));

    const selected = await vscode.window.showQuickPick(items, {
      placeHolder: "Select a graft connection",
    });

    return selected?.connection;
  }

  private async createProfile(connectionName: string): Promise<vscode.TerminalProfile> {
    const graft = await this.cli.resolvedPath();

    return new vscode.TerminalProfile({
      name: `Graft: ${connectionName}`,
      shellPath: graft,
      shellArgs: ["shell"],
      env: { GRAFT_SESSION: String(this.cli.pid) },
    });
  }
}
