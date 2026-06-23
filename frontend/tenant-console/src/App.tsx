import React from 'react';
import { SHARED_LIB_PLACEHOLDER } from '@ae-mdm/shared';

// tenant-console SPA のルート App コンポーネント。
// scaffold のみ。SuperAdmin 専用ルートは含まないこと（requirements.md 2.7 / NFR 2 参照）。
export default function App(): React.JSX.Element {
  return (
    <main className="min-h-screen p-8">
      <h1 className="text-2xl font-bold">ae-mdm Tenant Console</h1>
      <p className="mt-2 text-slate-600">
        Issue #1 scaffold. 顧客 IT 管理者向け SPA (TenantAdmin / Operator / Viewer)。
      </p>
      <p className="mt-1 text-xs text-slate-400">{SHARED_LIB_PLACEHOLDER}</p>
    </main>
  );
}
