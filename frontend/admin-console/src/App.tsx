import React from 'react';
import { SHARED_LIB_PLACEHOLDER } from '@ae-mdm/shared';

// admin-console SPA のルート App コンポーネント。
// scaffold のみ。テナント業務 UI（端末操作・ポリシー編集等）は含まないこと
// （requirements.md 2.7, NFR 2 参照、design.md「Web フロント構成（2 コンソール）」節）。
export default function App(): React.JSX.Element {
  return (
    <main className="min-h-screen p-8">
      <h1 className="text-2xl font-bold">ae-mdm Admin Console</h1>
      <p className="mt-2 text-slate-600">
        Issue #1 scaffold. SaaS 運用者向け SPA (SuperAdmin 専用)。
      </p>
      <p className="mt-1 text-xs text-slate-400">{SHARED_LIB_PLACEHOLDER}</p>
    </main>
  );
}
