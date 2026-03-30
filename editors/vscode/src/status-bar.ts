import * as vscode from "vscode";
import { ConnectionProvider } from "./connection-provider";

export class StatusBar implements vscode.Disposable {
  private readonly item: vscode.StatusBarItem;
  private readonly subscription: vscode.Disposable;

  constructor(private readonly provider: ConnectionProvider) {
    this.item = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
    this.item.command = "graft.selectConnection";
    this.update();
    this.item.show();

    this.subscription = provider.onDidChange(() => this.update());
  }

  private update() {
    if (!this.provider.daemonRunning) {
      this.item.text = "$(plug) Graft (stopped)";
      this.item.backgroundColor = new vscode.ThemeColor("statusBarItem.warningBackground");
      this.item.tooltip = "Graft daemon is not running";
      return;
    }

    this.item.backgroundColor = undefined;

    const current =
      this.provider.currentConnection() ??
      this.provider.connectionForWorkspace();
    if (!current) {
      this.item.text = "$(plug) Graft";
      const n = this.provider.getConnections().length;
      this.item.tooltip = n ? `Graft - ${n} connections` : "Graft - no connections";
      return;
    }

    const isError =
      current.state === "CONNECTION_STATE_FAILED" ||
      current.state === "CONNECTION_STATE_CLOSED";

    if (isError) {
      this.item.text = `$(plug) ${current.name} (!)`;
      this.item.backgroundColor = new vscode.ThemeColor("statusBarItem.warningBackground");
      this.item.tooltip = `Graft: ${current.name} - ${current.stateReason ?? current.state}`;
    } else {
      this.item.text = `$(plug) ${current.name}`;
      this.item.tooltip = `Graft: ${current.name} (${current.safeDestination})`;
    }
  }

  dispose() {
    this.subscription.dispose();
    this.item.dispose();
  }
}
