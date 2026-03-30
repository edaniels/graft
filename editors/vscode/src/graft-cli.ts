import { execFile } from "child_process";
import { promisify } from "util";
import * as vscode from "vscode";

const execFileAsync = promisify(execFile);

export interface ConnectionInfo {
  name: string;
  state: string;
  stateReason?: string;
  current: boolean;
  safeDestination: string;
}

export class GraftCli {
  private execPath: string | undefined;
  private resolvePromise: Promise<string> | undefined;
  readonly pid = process.pid;

  async resolvedPath(): Promise<string> {
    const configured = vscode.workspace
      .getConfiguration("graft")
      .get<string>("executablePath");
    if (configured) return configured;
    if (this.execPath) return this.execPath;

    if (!this.resolvePromise) {
      this.resolvePromise = this.resolveViaShell();
    }
    return this.resolvePromise;
  }

  private async resolveViaShell(): Promise<string> {
    const shell = process.env["SHELL"] ?? "/bin/sh";
    try {
      const { stdout } = await execFileAsync(shell, ["-lc", "which graft"], {
        timeout: 5000,
      });
      const resolved = stdout.trim();
      if (resolved) {
        this.execPath = resolved;
        return resolved;
      }
    } catch {
      // graft not found via shell
    }

    this.execPath = "graft";
    return "graft";
  }

  private async exec(args: string[]): Promise<string> {
    const bin = await this.resolvedPath();
    const env = { ...process.env, GRAFT_SESSION: String(this.pid) };
    const { stdout } = await execFileAsync(bin, args, { timeout: 10000, env });
    return stdout;
  }

  async listConnections(): Promise<ConnectionInfo[]> {
    const output = await this.exec(["status", "--json"]);
    const parsed = JSON.parse(output);
    const connections = parsed.connections ?? {};

    return Object.entries(connections).map(([name, status]: [string, any]) => ({
      name,
      state: status.state ?? "CONNECTION_STATE_UNKNOWN",
      stateReason: status.state_reason,
      current: status.current ?? false,
      safeDestination: status.safe_destination ?? "",
    }));
  }

  async pinConnection(name: string) {
    await this.exec(["use", name]);
  }

  async clearPin() {
    await this.exec(["use", "--clear"]);
  }

  async reportCwd(cwd: string) {
    await this.exec(["report-cwd", "--pid", String(this.pid), cwd]);
  }

  async stateDir(): Promise<string> {
    const output = await this.exec(["env"]);
    for (const line of output.split("\n")) {
      if (line.startsWith("GRAFT_STATE_HOME=")) {
        return line.slice("GRAFT_STATE_HOME=".length).replace(/^"|"$/g, "");
      }
    }
    throw new Error("GRAFT_STATE_HOME not found in graft env output");
  }
}
