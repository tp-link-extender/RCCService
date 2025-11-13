open System.IO
open Microsoft.FSharp.Core.Result
open System
open System.Diagnostics
open System.Net.Sockets

let (>>=) f x = bind x f

let log i =
    printfn $"[LOG] {i}"
    Ok i

let getStudio () =
    let path = "./staging/MercuryStudioBeta.exe"
    // check if the file exists
    match File.Exists path with
    | true -> Ok path
    | false -> Error $"File not found: {path}"

let getArgs (id: int) (path: string) =
    // get current operating system
    let args = [ path; "--script"; $"dofile(\"http://mercs.dev/game/{id}/serve\")" ]

    Ok(
        if OperatingSystem.IsLinux() then
            [ "wine" ] @ args
        else
            args
    )

let startGameserver (args: string list) =
    try
        let startInfo = ProcessStartInfo()
        startInfo.FileName <- args.Head
        startInfo.Arguments <- String.Join(" ", args.Tail)
        startInfo.UseShellExecute <- false
        startInfo.RedirectStandardOutput <- true
        startInfo.RedirectStandardError <- true

        let proc = Process.Start startInfo
        proc.EnableRaisingEvents <- true

        proc.OutputDataReceived.Add(fun data -> printfn $"[STDOUT] {data.Data}")
        // proc.ErrorDataReceived.Add(fun data -> printfn $"[STDERR] {data.Data}")

        proc.BeginOutputReadLine()
        proc.BeginErrorReadLine()

        Ok proc
    with ex ->
        Error $"Failed to start gameserver: {ex.Message}"

let pollMonitor (callback: unit -> unit) =
    let rec loop () =
        async {
            callback ()
            do! Async.Sleep 100
            return! loop ()
        }

    Async.Start(loop ())

let monitorGameserver (proc: Process) =
    proc.Exited.Add(fun _ -> printfn "[INFO] Gameserver process has exited.")

    // get when the process is responding
    pollMonitor (fun () ->
        // check if we can start a server on UDP port 53640
        let endpoint = Net.IPEndPoint(Net.IPAddress.Loopback, 53640)
        use socket = new Socket(AddressFamily.InterNetwork, SocketType.Dgram, ProtocolType.Udp)
        let canBind =
            try
                socket.Bind endpoint
                true
            with _ -> false
        
        if canBind then
            printfn "[INFO] Gameserver is not responding."
        else
            printfn "[INFO] Gameserver is responding."

        proc.Refresh()
        printfn $"  Physical memory usage     : {float proc.WorkingSet64 / 1e6}"
        printfn $"  Base priority             : {proc.BasePriority}"
        printfn $"  Priority class            : {proc.PriorityClass}"
        printfn $"  User processor time       : {proc.UserProcessorTime}"
        printfn $"  Privileged processor time : {proc.PrivilegedProcessorTime}"
        printfn $"  Total processor time      : {proc.TotalProcessorTime}"
        printfn $"  Paged system memory size  : {proc.PagedSystemMemorySize64}"
        printfn $"  Paged memory size         : {proc.PagedMemorySize64}")


    |> ignore

    // print whenever window title changes
    proc.WaitForExitAsync() |> Async.AwaitTask |> Async.RunSynchronously

    Ok()


let newGameserver (id: int) =
    getStudio ()
    >>= log
    >>= getArgs id
    >>= log
    >>= startGameserver
    >>= log
    >>= monitorGameserver

let start (id: int) =
    printfn $"Received start request for ID {id}"

    newGameserver id

// wait for all async tasks to complete

[<EntryPoint>]
let main _ =
    match start 42 with
    | Ok _ -> 0
    | Error msg ->
        printfn $"Error: {msg}"
        1
