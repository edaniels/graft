import { existsSync, readFileSync, watch, FSWatcher } from "fs";
import { join, sep } from "path";
import * as vscode from "vscode";
import { ConnectionInfo, GraftCli } from "./graft-cli";

export class ConnectionProvider implements vscode.Disposable {
  private readonly _onDidChange = new vscode.EventEmitter<void>();
  readonly onDidChange = this._onDidChange.event;

  private connections: ConnectionInfo[] = [];
  private _daemonRunning = false;
  private watcher: FSWatcher | undefined;
  private pollTimer: ReturnType<typeof setInterval> | undefined;
  private refreshTimer: ReturnType<typeof setTimeout> | undefined;
  private stateDir: string | undefined;

  constructor(private readonly cli: GraftCli) {}

  private async resolveStateDir(): Promise<string> {
    if (this.stateDir) return this.stateDir;
    this.stateDir = join(await this.cli.stateDir(), "local");
    this.tryWatch();
    return this.stateDir;
  }

  private tryWatch() {
    if (this.watcher || !this.stateDir) return;

    if (!existsSync(this.stateDir)) {
      if (!this.pollTimer) {
        this.pollTimer = setInterval(() => this.tryWatch(), 5000);
      }
      return;
    }

    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = undefined;
    }

    this.watcher = watch(this.stateDir, () => this.scheduleRefresh());
  }

  get daemonRunning() {
    return this._daemonRunning;
  }

  getConnections() {
    return this.connections;
  }

  currentConnection() {
    return this.connections.find((c) => c.current);
  }

  connectionForWorkspace(): ConnectionInfo | undefined {
    const folders = vscode.workspace.workspaceFolders;
    if (!folders?.length || !this.stateDir) return undefined;

    const roots = this.readConnectionRoots();
    for (const folder of folders) {
      const folderPath = folder.uri.fsPath;
      for (const [localRoot, connName] of roots) {
        if (folderPath === localRoot || folderPath.startsWith(localRoot + sep)) {
          return this.connections.find((c) => c.name === connName);
        }
      }
    }
    return undefined;
  }

  private readConnectionRoots(): Map<string, string> {
    const roots = new Map<string, string>();
    if (!this.stateDir) return roots;
    try {
      const content = readFileSync(join(this.stateDir, "connection_roots"), "utf-8");
      for (const line of content.trim().split("\n")) {
        const [path, conn] = line.split("\t", 2);
        if (path && conn) roots.set(path, conn);
      }
    } catch {
      // File doesn't exist yet
    }
    return roots;
  }

  private scheduleRefresh() {
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    this.refreshTimer = setTimeout(() => this.refresh(), 500);
  }

  async refresh() {
    try {
      await this.resolveStateDir();
      this.connections = await this.cli.listConnections();
      this._daemonRunning = true;
    } catch {
      this.connections = [];
      this._daemonRunning = false;
    }
    this._onDidChange.fire();
  }

  dispose() {
    this.watcher?.close();
    if (this.pollTimer) clearInterval(this.pollTimer);
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    this._onDidChange.dispose();
  }
}
