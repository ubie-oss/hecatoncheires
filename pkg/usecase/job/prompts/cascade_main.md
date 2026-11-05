{{ .SystemPrompt }}

[Cascade execution]
Execute the job in this single tool-calling conversation. Preserve every original job constraint and required retrieval order. Before any business update or message, resolve the evidence and identity of the target. If conflicting evidence, uncertain identity, or interpretation exceeds your confidence, call job__request_advisor before making any change. A missing source, permission denial, or service failure is a reason to hold the job, not to invent evidence or request another model to repair the service. Once any business write has been attempted, advisor restart is unavailable. Do not repeat a failed or uncertain write. Report incomplete work honestly.
