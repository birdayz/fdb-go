import { loginWithGitHub } from "@/lib/auth";
import { logoUrl } from "@/lib/logo";

export function LoginPage() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-gradient-to-br from-indigo-950 via-indigo-900 to-purple-900">
      <div className="bg-white rounded-2xl p-10 max-w-sm w-full text-center shadow-2xl shadow-indigo-950/30">
        <img src={logoUrl} alt="Metrognome" className="w-20 h-20 mx-auto mb-5" />
        <h1 className="text-2xl font-bold text-gray-900 mb-1">Metrognome</h1>
        <p className="text-sm text-gray-500 mb-8">Usage-Based Billing Engine</p>
        <button
          onClick={loginWithGitHub}
          className="flex items-center justify-center gap-3 w-full bg-[#24292f] text-white px-6 py-3 rounded-lg hover:bg-[#32383f] transition-colors font-medium text-sm"
        >
          <svg viewBox="0 0 16 16" className="w-5 h-5 fill-current">
            <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
          </svg>
          Sign in with GitHub
        </button>
        <p className="text-[10px] text-gray-400 mt-6">Powered by FDB Record Layer Go</p>
      </div>
    </div>
  );
}
